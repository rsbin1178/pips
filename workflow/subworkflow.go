package workflow

import (
	"context"
	"errors"
)

// SubWorkflowConfig pins the child Workflow executed by a SubWorkflow node.
type SubWorkflowConfig struct {
	Workflow DefinitionRef `json:"workflow"`
}

// SubWorkflowNode synchronously executes one exact referenced Workflow.
type SubWorkflowNode struct{}

// Spec implements [NodeType].
func (SubWorkflowNode) Spec() NodeTypeSpec {
	return NodeTypeSpec{
		Key:         NodeTypeSubWorkflow,
		Version:     BuiltinNodeVersion,
		DisplayName: "SubWorkflow",
	}
}

// Compile implements [NodeType].
func (SubWorkflowNode) Compile(
	ctx context.Context,
	compileContext CompileContext,
	definition NodeDefinition,
) (CompiledNode, error) {
	var config SubWorkflowConfig
	if err := decodeNodeConfig(definition, &config); err != nil {
		return nil, err
	}

	compositeContext, ok := compileContext.(compositeCompileContext)
	if !ok {
		return nil, compileNodeError(definition.ID, "composite compiler is unavailable")
	}

	child, err := compositeContext.resolveDefinition(ctx, config.Workflow)
	if err != nil {
		return nil, wrapCompileNodeError(definition.ID, "sub-workflow", err)
	}

	outputs := make(map[string]PortSchema, len(child.definition.Outputs))
	for name, output := range child.definition.Outputs {
		outputs[name] = output.Schema
	}

	return &compiledSubWorkflow{
		spec: NodeSpec{
			Inputs:  cloneSchemas(child.definition.Inputs),
			Outputs: outputs,
			Routes:  []string{RouteSuccess},
		},
		child: child,
	}, nil
}

type compiledSubWorkflow struct {
	spec  NodeSpec
	child *Plan
}

func (n *compiledSubWorkflow) Spec() NodeSpec {
	return cloneNodeSpec(n.spec)
}

func (n *compiledSubWorkflow) Invoke(ctx context.Context, input NodeInput) (NodeOutput, error) {
	if input.runtime == nil {
		return NodeOutput{}, errors.New("sub-workflow runtime is unavailable")
	}

	outputs, err := input.runtime.runChild(
		ctx,
		n.child,
		input.Values,
		ScopeFrame{Kind: ScopeSubWorkflow, NodeID: input.runtime.nodeID, Index: -1},
	)
	if err != nil {
		return NodeOutput{}, err
	}

	return NodeOutput{Values: outputs, Route: RouteSuccess}, nil
}

func (n *compiledSubWorkflow) childPlans() []*Plan {
	return []*Plan{n.child}
}

var (
	_ CompiledNode          = (*compiledSubWorkflow)(nil)
	_ compiledCompositeNode = (*compiledSubWorkflow)(nil)
)
