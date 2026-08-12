package workflow

import (
	"errors"
	"reflect"
	"testing"
)

func TestCompileErrorContract(t *testing.T) {
	t.Parallel()

	cause := errors.New("sentinel cause")
	node := newNodeCompileIssue(
		CompileIssueUnknownNodeType,
		NewNodePath("outer", "missing"),
		`node "outer/missing": unknown node type "custom"@"v2"`,
	)
	path := newControlPathCompileIssue(
		CompileIssueInvalidControlPath,
		CompileControlPath{
			From:  NewNodePath("outer", "source"),
			To:    NewNodePath("outer", "target"),
			Route: "success",
		},
		`control path "outer/source"/"success" -> "outer/target": duplicate control edge`,
	)

	compileErr := newCompileError([]CompileIssue{node, path}, cause)
	if compileErr == nil {
		t.Fatal("newCompileError() = nil")
	}

	if !errors.Is(compileErr, ErrCompile) || !errors.Is(compileErr, cause) {
		t.Fatalf("CompileError chain = %v, want ErrCompile and cause", compileErr)
	}

	var extracted *CompileError
	if !errors.As(compileErr, &extracted) || extracted != compileErr {
		t.Fatalf("errors.As() = %#v, want original CompileError", extracted)
	}

	wantError := "workflow: compile failed: [unknown_node_type] " +
		`node "outer/missing": unknown node type "custom"@"v2"; ` +
		"[invalid_control_path] " +
		`control path "outer/source"/"success" -> "outer/target": duplicate control edge`
	if got := compileErr.Error(); got != wantError {
		t.Fatalf("CompileError.Error() = %q, want %q", got, wantError)
	}

	issues := compileErr.Issues()
	if len(issues) != 2 {
		t.Fatalf("CompileError.Issues() len = %d, want 2", len(issues))
	}

	if issues[0].Code() != CompileIssueUnknownNodeType ||
		issues[0].Location() != CompileLocationNode {
		t.Fatalf("first issue = %#v", issues[0])
	}

	nodePath, ok := issues[0].NodePath()
	if !ok || !reflect.DeepEqual(nodePath.Nodes(), []NodeID{"outer", "missing"}) {
		t.Fatalf("first NodePath() = %v, %t", nodePath.Nodes(), ok)
	}

	if _, ok := issues[0].ControlPath(); ok {
		t.Fatal("node issue ControlPath() ok = true")
	}

	controlPath, ok := issues[1].ControlPath()
	if !ok || !reflect.DeepEqual(controlPath.From.Nodes(), []NodeID{"outer", "source"}) ||
		!reflect.DeepEqual(controlPath.To.Nodes(), []NodeID{"outer", "target"}) ||
		controlPath.Route != RouteSuccess {
		t.Fatalf("second ControlPath() = %#v, %t", controlPath, ok)
	}

	if _, ok := issues[1].NodePath(); ok {
		t.Fatal("path issue NodePath() ok = true")
	}
}

func TestCompileErrorAccessorsReturnDetachedValues(t *testing.T) {
	t.Parallel()

	compileErr := newCompileError([]CompileIssue{
		newControlPathCompileIssue(
			CompileIssueInvalidControlPath,
			CompileControlPath{
				From:  NewNodePath("source"),
				To:    NewNodePath("target"),
				Route: RouteSuccess,
			},
			"bad edge",
		),
	})
	wantError := compileErr.Error()

	issues := compileErr.Issues()
	issues[0] = CompileIssue{}

	second := compileErr.Issues()

	path, ok := second[0].ControlPath()
	if !ok {
		t.Fatal("ControlPath() ok = false")
	}

	from := path.From.Nodes()
	to := path.To.Nodes()
	from[0] = "changed-source"
	to[0] = "changed-target"

	again, ok := compileErr.Issues()[0].ControlPath()
	if !ok || !reflect.DeepEqual(again.From.Nodes(), []NodeID{"source"}) ||
		!reflect.DeepEqual(again.To.Nodes(), []NodeID{"target"}) {
		t.Fatalf("mutated control path = %#v", again)
	}

	if got := compileErr.Error(); got != wantError {
		t.Fatalf("CompileError.Error() after caller mutation = %q, want %q", got, wantError)
	}

	unwrapped := compileErr.Unwrap()
	unwrapped[0] = errors.New("changed")

	if !errors.Is(compileErr, ErrCompile) {
		t.Fatal("caller mutation changed CompileError unwrap chain")
	}
}

func TestCompileErrorZeroValuesAreSafe(t *testing.T) {
	t.Parallel()

	var issue CompileIssue
	if issue.Code() != "" || issue.Message() != "" || issue.Location() != "" {
		t.Fatalf("zero CompileIssue = %#v", issue)
	}

	if _, ok := issue.NodePath(); ok {
		t.Fatal("zero CompileIssue.NodePath() ok = true")
	}

	if _, ok := issue.ControlPath(); ok {
		t.Fatal("zero CompileIssue.ControlPath() ok = true")
	}

	var compileErr *CompileError
	if compileErr.Error() != ErrCompile.Error() || compileErr.Issues() != nil ||
		!errors.Is(compileErr, ErrCompile) {
		t.Fatalf("nil CompileError contract = %q, %#v", compileErr.Error(), compileErr.Issues())
	}

	if newCompileError(nil) != nil {
		t.Fatal("newCompileError(nil) != nil")
	}
}
