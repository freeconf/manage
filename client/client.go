package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"time"

	"github.com/freeconf/restconf"
	"github.com/freeconf/restconf/device"
	"github.com/freeconf/yang/fc"
	"github.com/freeconf/yang/meta"
	"github.com/freeconf/yang/node"
	"github.com/freeconf/yang/nodeutil"
	"github.com/freeconf/yang/parser"
	"github.com/freeconf/yang/source"
)

// NewClient interfaces with a remote RESTCONF server.  This also implements device.Device
// making it appear like a local device and is important architecturaly.  Code that uses
// this in a node.Browser context would not know the difference from a remote or local device
// with one minor exceptions. Peek() wouldn't work.
type Client struct {
	YangPath  source.Opener
	Complance restconf.ComplianceOptions
}

func ProtocolHandler(ypath source.Opener) device.ProtocolHandler {
	c := Client{YangPath: ypath}
	return c.NewDevice
}

type Address struct {
	Base       string
	Data       string
	Stream     string
	Ui         string
	Operations string
	Schema     string
	DeviceId   string
	Host       string
	Origin     string
}

func NewAddress(urlAddr string) (Address, error) {
	// remove trailing '/' if there is one to prepare for appending
	if urlAddr[len(urlAddr)-1] != '/' {
		urlAddr = urlAddr + "/"
	}

	urlParts, err := url.Parse(urlAddr)
	if err != nil {
		return Address{}, err
	}

	return Address{
		Base:       urlAddr,
		Data:       urlAddr + "data/",
		Schema:     urlAddr + "schema/",
		Ui:         urlAddr + "ui/",
		Operations: urlAddr + "operations/",
		Origin:     "http://" + urlParts.Host,
		DeviceId:   restconf.FindDeviceIdInUrl(urlAddr),
	}, nil
}

func (factory Client) NewDevice(url string) (device.Device, error) {
	address, err := NewAddress(url)
	if err != nil {
		return nil, err
	}
	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
	}
	remoteSchemaPath := httpStream{
		ypath:  factory.YangPath,
		client: httpClient,
		url:    address.Schema,
	}
	c := &client{
		address:    address,
		yangPath:   factory.YangPath,
		schemaPath: source.Any(factory.YangPath, remoteSchemaPath.OpenStream),
		client:     httpClient,
		compliance: factory.Complance,
	}
	d := &clientNode{support: c, device: address.DeviceId, compliance: c.compliance}
	m := parser.RequireModule(factory.YangPath, "ietf-yang-library")
	b := node.NewBrowser(m, d.node())
	modules, err := device.LoadModules(b, remoteSchemaPath)
	if err != nil {
		return nil, fmt.Errorf("could not load modules. %s", err)
	}
	fc.Debug.Printf("loaded modules %v", modules)
	c.modules = modules
	return c, nil
}

type client struct {
	address    Address
	yangPath   source.Opener
	schemaPath source.Opener
	client     *http.Client
	modules    map[string]*meta.Module
	compliance restconf.ComplianceOptions
}

func (c *client) SchemaSource() source.Opener {
	return c.schemaPath
}

func (c *client) UiSource() source.Opener {
	s := httpStream{
		client: c.client,
		url:    c.address.Ui,
	}
	return s.OpenStream
}

func (c *client) Browser(module string) (*node.Browser, error) {
	d := &clientNode{support: c, device: c.address.DeviceId, compliance: c.compliance}
	m, err := c.module(module)
	if err != nil {
		return nil, err
	}
	return node.NewBrowser(m, d.node()), nil
}

func (c *client) Close() {
}

func (c *client) Modules() map[string]*meta.Module {
	return c.modules
}

func (c *client) module(module string) (*meta.Module, error) {
	// caching module, but should replace w/cache that can refresh on stale
	m := c.modules[module]
	if m == nil {
		var err error
		if m, err = parser.LoadModule(c.schemaPath, module); err != nil {
			return nil, err
		}
		c.modules[module] = m
	}
	return m, nil
}

type streamEvent struct {
	Timestamp time.Time
	Node      node.Node
}

func (c *client) clientStream(params string, p *node.Path, ctx context.Context) (<-chan streamEvent, error) {
	mod := meta.RootModule(p.Meta)
	fullUrl := fmt.Sprint(c.address.Data, mod.Ident(), ":", p.StringNoModule())
	req, err := http.NewRequest("GET", fullUrl, nil)
	if err != nil {
		return nil, err
	}
	if c.compliance == restconf.Simplified {
		q := req.URL.Query()
		q.Add(restconf.SimplifiedComplianceParam, "")
		req.URL.RawQuery = q.Encode()
	}
	req.Header.Set("Accept", string(restconf.TextStreamMimeType))
	fc.Debug.Printf("<=> SSE %s", fullUrl)
	stream := make(chan streamEvent)
	go func() {
		resp, err := c.client.Do(req)
		if err != nil {
			stream <- streamEvent{
				Timestamp: time.Now(),
				Node:      node.ErrorNode{Err: err},
			}
			return
		}
		events := decodeSse(resp.Body)
		defer resp.Body.Close()
		for {
			select {
			case event := <-events:
				var e streamEvent
				var vals map[string]interface{}
				err := json.Unmarshal(event, &vals)
				if err == nil {
					if !c.compliance.DisableNotificationWrapper {
						payload, found := vals["ietf-restconf:notification"].(map[string]interface{})
						if !found {
							err = errors.New("SSE message missing ietf-restconf:notification wrapper")
						} else {
							body, found := payload["event"].(map[string]interface{})
							if !found {
								err = errors.New("SSE message missing event payload")
							} else {
								tstr, found := payload["eventTime"].(string)
								if !found {
									err = errors.New("SSE message missing eventTime")
								} else {
									var t time.Time
									t, err = time.Parse(restconf.EventTimeFormat, tstr)
									if err != nil {
										err = fmt.Errorf("eventTime in wrong format '%s'", tstr)
									} else {
										e = streamEvent{
											Timestamp: t,
											Node:      nodeutil.ReadJSONValues(body),
										}
									}
								}
							}
						}
					} else {
						e = streamEvent{
							Node:      nodeutil.ReadJSONIO(bytes.NewReader(event)),
							Timestamp: time.Now(),
						}
					}
				}
				if err != nil {
					e = streamEvent{
						Node:      node.ErrorNode{Err: err},
						Timestamp: time.Now(),
					}
				}
				stream <- e
			case <-ctx.Done():
				return
			}
		}
	}()

	return stream, nil
}

// ClientSchema downloads schema and implements yang.StreamSource so it can transparently
// be used in a YangPath.
type httpStream struct {
	ypath  source.Opener
	client *http.Client
	url    string
}

func (s httpStream) ResolveModuleHnd(hnd device.ModuleHnd) (*meta.Module, error) {
	m, _ := parser.LoadModule(s.ypath, hnd.Name)
	if m != nil {
		return m, nil
	}
	return parser.LoadModule(s.OpenStream, hnd.Name)
}

// OpenStream implements source.Opener
func (s httpStream) OpenStream(name string, ext string) (io.Reader, error) {
	fullUrl := s.url + name + ext
	fc.Debug.Printf("httpStream url %s, name=%s, ext=%s", fullUrl, name, ext)
	resp, err := s.client.Get(fullUrl)
	if resp != nil {
		return resp.Body, err
	}
	return nil, err
}

func (c *client) clientDo(method string, params string, p *node.Path, payload io.Reader) (io.ReadCloser, error) {
	var req *http.Request
	var err error
	mod := meta.RootModule(p.Meta)
	fullUrl := fmt.Sprint(c.address.Data, mod.Ident(), ":", p.StringNoModule())

	if meta.IsAction(p.Meta) && !c.compliance.AllowRpcUnderData {
		isRootLevelRpc := (p.Meta.Parent() == mod)
		if isRootLevelRpc {
			fullUrl = fmt.Sprint(c.address.Operations, mod.Ident(), ":", p.StringNoModule())
		}
	}
	if params != "" {
		fullUrl = fmt.Sprint(fullUrl, "?", params)
	}
	if req, err = http.NewRequest(method, fullUrl, payload); err != nil {
		return nil, err
	}
	if c.compliance == restconf.Simplified {
		req.Header.Set("Content-Type", string(restconf.PlainJsonMimeType))
		req.Header.Set("Accept", string(restconf.PlainJsonMimeType))
	} else {
		req.Header.Set("Content-Type", string(restconf.YangDataJsonMimeType1))
		req.Header.Set("Accept", string(restconf.YangDataJsonMimeType1))
	}
	fc.Debug.Printf("=> %s %s", method, fullUrl)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		msg, _ := ioutil.ReadAll(resp.Body)
		return nil, fmt.Errorf("(%d) %s", resp.StatusCode, string(msg))
	}
	if resp.Body == nil || resp.ContentLength == 0 {
		return nil, nil
	}
	return resp.Body, nil
}
