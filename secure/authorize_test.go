package secure

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/freeconf/yang/val"

	"github.com/freeconf/yang/fc"
	"github.com/freeconf/yang/node"
	"github.com/freeconf/yang/nodeutil"
	"github.com/freeconf/yang/parser"
)

type testAc struct {
	path string
	perm Permission
}

const (
	xAllowed string = "allowed"
	xHidden         = "hidden"
	xUnauth         = "unauthorized"
)

func TestAuthConstraints(t *testing.T) {
	fc.DebugLog(true)
	m, err := parser.LoadModuleFromString(nil, `module birding { revision 0;
leaf count {
	type int32;
}
container owner {
	leaf name {
		type string;
	}
}
action fieldtrip {
	input {}
}
notification identified {}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	dataStr := `{
		"count" : 10,
		"owner": {"name":"ethel"}
	}`
	var data map[string]interface{}
	if err := json.NewDecoder(strings.NewReader(dataStr)).Decode(&data); err != nil {
		panic(err)
	}
	n := &nodeutil.Extend{
		Base: nodeutil.ReflectChild(data),
		OnNotify: func(p node.Node, r node.NotifyRequest) (node.NotifyCloser, error) {
			r.Send(&nodeutil.Basic{})
			closer := func() error { return nil }
			return closer, nil
		},
		OnAction: func(p node.Node, r node.ActionRequest) (node.Node, error) {
			return nil, nil
		},
	}
	b := node.NewBrowser(m, n)
	tests := []struct {
		desc      string
		acls      []*AccessControl
		read      string
		readPath  string
		write     string
		writePath string
		notify    string
		action    string
	}{
		{
			desc: "default",
			acls: []*AccessControl{
				/* empty */
			},
			read:      xHidden,
			readPath:  xHidden,
			write:     xUnauth,
			writePath: xHidden,
			notify:    xUnauth,
			action:    xUnauth,
		},
		{
			desc: "none",
			acls: []*AccessControl{
				{
					Path:        "birding",
					Permissions: Read,
				},
			},
			read:      xAllowed,
			readPath:  xAllowed,
			write:     xUnauth,
			writePath: xUnauth,
			notify:    xUnauth,
			action:    xUnauth,
		},
		{
			desc: "full",
			acls: []*AccessControl{
				{
					Path:        "birding",
					Permissions: Full,
				},
			},
			read:      xAllowed,
			readPath:  xAllowed,
			write:     xAllowed,
			writePath: xAllowed,
			notify:    xAllowed,
			action:    xAllowed,
		},
		{
			desc: "mixed",
			acls: []*AccessControl{
				{
					Path:        "birding",
					Permissions: Full,
				},
				{
					Path:        "birding/owner",
					Permissions: None,
				},
			},
			read:      xAllowed,
			readPath:  xHidden,
			write:     xAllowed,
			writePath: xHidden,
			notify:    xAllowed,
			action:    xAllowed,
		},
	}
	for _, test := range tests {
		acl := NewRole()
		for _, testAcDef := range test.acls {
			acl.Access[testAcDef.Path] = testAcDef
		}

		s := b.Root()
		s.Constraints.AddConstraint("auth", 0, 0, acl)
		s.Context = s.Constraints.ContextConstraint(s)

		t.Log(test.desc + " read")
		fc.AssertEqual(t, test.read, val2auth(sel(s.Find("count")).Get()))

		t.Logf(test.desc + " read path")
		pathSel := sel(s.Find("owner"))
		fc.AssertEqual(t, test.readPath, sel2auth(pathSel))

		t.Log(test.desc + " write")
		writeErr := sel(s.Find("count")).SetValue(100)
		fc.AssertEqual(t, test.write, err2auth(writeErr))

		t.Log(test.desc + " write path")
		if pathSel == nil {
			fc.AssertEqual(t, test.writePath, xHidden)
		} else {
			writePathErr := sel(pathSel.Find("name")).SetValue("Harvey")
			fc.AssertEqual(t, test.writePath, err2auth(writePathErr))
		}

		t.Log(test.desc + " execute")
		_, actionErr := sel(s.Find("fieldtrip")).Action(nil)
		fc.AssertEqual(t, test.action, err2auth(actionErr))

		t.Log(test.desc + " notify")
		var notifyErr error
		sel(s.Find("identified")).Notifications(func(n node.Notification) {
			if errNode, isErr := (n.Event.Node).(node.ErrorNode); isErr {
				notifyErr = errNode.Err
			}
		})
		fc.AssertEqual(t, test.notify, err2auth(notifyErr))
	}
}

func val2auth(v val.Value, err error) string {
	if v == nil {
		return xHidden
	}
	if errors.Is(err, fc.UnauthorizedError) {
		return xUnauth
	}
	return xAllowed
}

func sel2auth(s *node.Selection) string {
	if s == nil {
		return xHidden
	}
	return xAllowed
}

func err2auth(err error) string {
	if err == nil {
		return xAllowed
	} else if errors.Is(err, fc.UnauthorizedError) {
		return xUnauth
	}
	panic(err.Error())
}

func sel(sel *node.Selection, err error) *node.Selection {
	if err != nil {
		panic(err)
	}
	return sel
}
