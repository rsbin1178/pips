package workflow

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// CompileIssueCode identifies a stable compile failure category. Messages may
// become more specific over time; callers should branch on Code instead.
type CompileIssueCode string

// Compile issue codes.
const (
	CompileIssueInvalidDefinition  CompileIssueCode = "invalid_definition"
	CompileIssueInvalidInterrupt   CompileIssueCode = "invalid_interrupt"
	CompileIssueInvalidReference   CompileIssueCode = "invalid_reference"
	CompileIssueUnknownNodeType    CompileIssueCode = "unknown_node_type"
	CompileIssueInvalidNodeConfig  CompileIssueCode = "invalid_node_config"
	CompileIssueInvalidNodeSpec    CompileIssueCode = "invalid_node_spec"
	CompileIssueInvalidControlPath CompileIssueCode = "invalid_control_path"
	CompileIssueInvalidGraph       CompileIssueCode = "invalid_graph"
	CompileIssueInvalidBinding     CompileIssueCode = "invalid_binding"
)

// CompileIssueLocation identifies which location accessor is available on a
// CompileIssue.
type CompileIssueLocation string

// Compile issue locations.
const (
	CompileLocationDefinition CompileIssueLocation = "definition"
	CompileLocationNode       CompileIssueLocation = "node"
	CompileLocationPath       CompileIssueLocation = "path"
)

// CompileControlPath identifies one control connection in a root or nested
// Definition. From and To are complete paths through composite boundaries.
type CompileControlPath struct {
	From  NodePath
	To    NodePath
	Route string
}

// CompileIssue is one immutable, process-local compiler diagnostic. It has no
// prescribed JSON or persistence format; hosts own versioned projections.
type CompileIssue struct {
	code     CompileIssueCode
	message  string
	location CompileIssueLocation
	node     NodePath
	control  CompileControlPath
	_        [0]func()
}

// Code returns the stable category of i.
func (i CompileIssue) Code() CompileIssueCode {
	return i.code
}

// Message returns the human-readable detail of i.
func (i CompileIssue) Message() string {
	return i.message
}

// Location returns the location kind of i.
func (i CompileIssue) Location() CompileIssueLocation {
	return i.location
}

// NodePath returns the complete node path when i is node-located.
func (i CompileIssue) NodePath() (NodePath, bool) {
	if i.location != CompileLocationNode {
		return NodePath{}, false
	}

	return cloneNodePath(i.node), true
}

// ControlPath returns the complete control path when i is path-located.
func (i CompileIssue) ControlPath() (CompileControlPath, bool) {
	if i.location != CompileLocationPath {
		return CompileControlPath{}, false
	}

	return cloneCompileControlPath(i.control), true
}

// CompileError reports one or more ordered issues that prevented Plan
// construction. Compile aggregates only the earliest failed dependency phase;
// later phases are not evaluated after their prerequisites fail.
type CompileError struct {
	issues []CompileIssue
	causes []error
}

// Error returns a deterministic summary of every issue.
func (e *CompileError) Error() string {
	if e == nil || len(e.issues) == 0 {
		return ErrCompile.Error()
	}

	var builder strings.Builder
	builder.WriteString(ErrCompile.Error())
	builder.WriteString(": ")

	for index, issue := range e.issues {
		if index > 0 {
			builder.WriteString("; ")
		}

		fmt.Fprintf(&builder, "[%s] %s", issue.code, issue.message)
	}

	return builder.String()
}

// Unwrap preserves ErrCompile and any underlying Definition, resolver, or
// NodeType causes for errors.Is and errors.As.
func (e *CompileError) Unwrap() []error {
	unwrapped := []error{ErrCompile}
	if e == nil {
		return unwrapped
	}

	return append(unwrapped, slices.Clone(e.causes)...)
}

// Issues returns an independent snapshot of the ordered diagnostics.
func (e *CompileError) Issues() []CompileIssue {
	if e == nil {
		return nil
	}

	return cloneCompileIssues(e.issues)
}

type compileIssueError struct {
	issue CompileIssue
	cause error
}

type compileErrorCollector struct {
	issues []CompileIssue
	causes []error
}

func (e *compileIssueError) Error() string {
	if e == nil {
		return ErrCompile.Error()
	}

	return fmt.Sprintf("%s: %s", ErrCompile, e.issue.message)
}

func (e *compileIssueError) Unwrap() []error {
	unwrapped := []error{ErrCompile}
	if e != nil && e.cause != nil {
		unwrapped = append(unwrapped, e.cause)
	}

	return unwrapped
}

func newDefinitionCompileIssue(code CompileIssueCode, message string) CompileIssue {
	return CompileIssue{
		code: code, message: message, location: CompileLocationDefinition,
	}
}

func newNodeCompileIssue(
	code CompileIssueCode,
	path NodePath,
	message string,
) CompileIssue {
	return CompileIssue{
		code: code, message: message, location: CompileLocationNode,
		node: cloneNodePath(path),
	}
}

func newControlPathCompileIssue(
	code CompileIssueCode,
	path CompileControlPath,
	message string,
) CompileIssue {
	return CompileIssue{
		code: code, message: message, location: CompileLocationPath,
		control: cloneCompileControlPath(path),
	}
}

func newCompileError(issues []CompileIssue, causes ...error) *CompileError {
	if len(issues) == 0 {
		return nil
	}

	filtered := make([]error, 0, len(causes))
	for _, cause := range causes {
		if cause == nil || errors.Is(ErrCompile, cause) {
			continue
		}

		if containsEquivalentCause(filtered, cause) {
			continue
		}

		filtered = append(filtered, cause)
	}

	return &CompileError{
		issues: cloneCompileIssues(issues),
		causes: slices.Clone(filtered),
	}
}

func containsEquivalentCause(causes []error, candidate error) bool {
	for _, cause := range causes {
		if errors.Is(cause, candidate) && errors.Is(candidate, cause) {
			return true
		}
	}

	return false
}

func compileFailureForIssue(issue CompileIssue, cause error) error {
	return &compileIssueError{issue: cloneCompileIssue(issue), cause: cause}
}

func compileErrorFromFailure(
	err error,
	fallback CompileIssue,
) *CompileError {
	if err == nil {
		return nil
	}

	var compileErr *CompileError
	if errors.As(err, &compileErr) {
		return cloneCompileError(compileErr)
	}

	var failure *compileIssueError
	if errors.As(err, &failure) {
		causes := make([]error, 0, 1)
		if failure.cause != nil {
			causes = append(causes, failure.cause)
		}

		return newCompileError([]CompileIssue{failure.issue}, causes...)
	}

	if fallback.message == "" {
		fallback.message = err.Error()
	}

	return newCompileError([]CompileIssue{fallback}, err)
}

func (c *compileErrorCollector) add(err error, fallback CompileIssue) {
	compileErr := compileErrorFromFailure(err, fallback)
	if compileErr == nil {
		return
	}

	c.issues = append(c.issues, cloneCompileIssues(compileErr.issues)...)
	c.causes = append(c.causes, compileErr.causes...)
}

func (c *compileErrorCollector) addIssue(issue CompileIssue, cause error) {
	c.issues = append(c.issues, cloneCompileIssue(issue))
	if cause != nil {
		c.causes = append(c.causes, cause)
	}
}

func (c *compileErrorCollector) err() error {
	if len(c.issues) == 0 {
		return nil
	}

	return newCompileError(c.issues, c.causes...)
}

func cloneCompileError(source *CompileError) *CompileError {
	if source == nil {
		return nil
	}

	return &CompileError{
		issues: cloneCompileIssues(source.issues),
		causes: slices.Clone(source.causes),
	}
}

func cloneCompileIssues(source []CompileIssue) []CompileIssue {
	cloned := make([]CompileIssue, len(source))
	for index, issue := range source {
		cloned[index] = cloneCompileIssue(issue)
	}

	return cloned
}

func cloneCompileIssue(source CompileIssue) CompileIssue {
	source.node = cloneNodePath(source.node)
	source.control = cloneCompileControlPath(source.control)

	return source
}

func cloneCompileControlPath(source CompileControlPath) CompileControlPath {
	return CompileControlPath{
		From:  cloneNodePath(source.From),
		To:    cloneNodePath(source.To),
		Route: source.Route,
	}
}

func cloneNodePath(source NodePath) NodePath {
	return NewNodePath(source.nodes...)
}

func nodeCompileMessage(path NodePath, detail string) string {
	return fmt.Sprintf("node %q: %s", pathString(path), detail)
}

func controlPathCompileMessage(path CompileControlPath, detail string) string {
	return fmt.Sprintf(
		"control path %q/%q -> %q: %s",
		pathString(path.From),
		path.Route,
		pathString(path.To),
		detail,
	)
}

func compileDefinitionFailure(
	code CompileIssueCode,
	cause error,
	format string,
	values ...any,
) error {
	message := fmt.Sprintf(format, values...)

	return compileFailureForIssue(newDefinitionCompileIssue(code, message), cause)
}

func compileNodeFailure(
	code CompileIssueCode,
	nodeID NodeID,
	cause error,
	format string,
	values ...any,
) error {
	path := NewNodePath(nodeID)
	detail := fmt.Sprintf(format, values...)

	return compileFailureForIssue(
		newNodeCompileIssue(code, path, nodeCompileMessage(path, detail)),
		cause,
	)
}

func compileControlPathFailure(
	code CompileIssueCode,
	edge ControlEdge,
	cause error,
	format string,
	values ...any,
) error {
	path := CompileControlPath{
		From:  NewNodePath(edge.From.Node),
		To:    NewNodePath(edge.To),
		Route: edge.From.Route,
	}
	detail := fmt.Sprintf(format, values...)

	return compileFailureForIssue(
		newControlPathCompileIssue(code, path, controlPathCompileMessage(path, detail)),
		cause,
	)
}

func prefixCompileError(nodeID NodeID, operation string, err error) error {
	if err == nil {
		return nil
	}

	fallback := newDefinitionCompileIssue(
		CompileIssueInvalidNodeConfig,
		err.Error(),
	)

	compileErr := compileErrorFromFailure(err, fallback)
	if compileErr == nil {
		return nil
	}

	issues := make([]CompileIssue, len(compileErr.issues))
	for index, issue := range compileErr.issues {
		issues[index] = prefixCompileIssue(nodeID, operation, issue)
	}

	return newCompileError(issues, compileErr.causes...)
}

func prefixCompileIssue(nodeID NodeID, operation string, issue CompileIssue) CompileIssue {
	detail := issue.message
	if operation != "" {
		detail = operation + ": " + detail
	}

	ownerPath := NewNodePath(nodeID)
	message := nodeCompileMessage(ownerPath, detail)

	switch issue.location {
	case CompileLocationNode:
		path := prependNodePath(nodeID, issue.node)

		return newNodeCompileIssue(issue.code, path, message)
	case CompileLocationPath:
		path := CompileControlPath{
			From:  prependNodePath(nodeID, issue.control.From),
			To:    prependNodePath(nodeID, issue.control.To),
			Route: issue.control.Route,
		}

		return newControlPathCompileIssue(issue.code, path, message)
	default:
		path := NewNodePath(nodeID)

		return newNodeCompileIssue(issue.code, path, message)
	}
}

func prependNodePath(nodeID NodeID, path NodePath) NodePath {
	nodes := make([]NodeID, 0, len(path.nodes)+1)
	nodes = append(nodes, nodeID)
	nodes = append(nodes, path.nodes...)

	return NewNodePath(nodes...)
}
