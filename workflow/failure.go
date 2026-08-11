package workflow

import (
	"errors"
	"fmt"
	"maps"
)

const (
	// NodeErrorMessagePort contains the terminal error text after retry exhaustion.
	NodeErrorMessagePort = "error_message"
	// NodeErrorTypePort contains the stable [FailureKind] classification.
	NodeErrorTypePort = "error_type"
)

type nodeFailureData map[string]Value

func validNodeErrorPort(port string) bool {
	return port == NodeErrorMessagePort || port == NodeErrorTypePort
}

func nodeErrorPortSchema(port string) (PortSchema, error) {
	schema := `{"type":"string"}`
	if port == NodeErrorTypePort {
		schema = `{"type":"string","enum":["error","timeout","panic","canceled","limit"]}`
	}

	if !validNodeErrorPort(port) {
		return PortSchema{}, fmt.Errorf("unknown node error port %q", port)
	}

	parsed, err := ParsePortSchema([]byte(schema))
	if err != nil {
		return PortSchema{}, fmt.Errorf("prepare node error port %q: %w", port, err)
	}

	return parsed, nil
}

func newNodeFailureData(message string, kind FailureKind) (nodeFailureData, error) {
	messageValue, err := ValueOf(message)
	if err != nil {
		return nil, fmt.Errorf("materialize node error message: %w", err)
	}

	typeValue, err := ValueOf(string(kind))
	if err != nil {
		return nil, fmt.Errorf("materialize node error type: %w", err)
	}

	return nodeFailureData{
		NodeErrorMessagePort: messageValue,
		NodeErrorTypePort:    typeValue,
	}, nil
}

func cloneNodeFailureData(data nodeFailureData) nodeFailureData {
	if data == nil {
		return nil
	}

	cloned := make(nodeFailureData, len(data))
	maps.Copy(cloned, data)

	return cloned
}

func validateNodeFailureData(data nodeFailureData, kind FailureKind) error {
	if len(data) != 2 {
		return errors.New("node failure data must contain exactly two ports")
	}

	for _, port := range []string{NodeErrorMessagePort, NodeErrorTypePort} {
		value, ok := data[port]
		if !ok {
			return fmt.Errorf("node failure data is missing port %q", port)
		}

		schema, err := nodeErrorPortSchema(port)
		if err != nil {
			return err
		}

		if err := schema.Validate(value); err != nil {
			return fmt.Errorf("node failure port %q: %w", port, err)
		}
	}

	typeName, err := DecodeValue[string](data[NodeErrorTypePort])
	if err != nil {
		return fmt.Errorf("decode node error type: %w", err)
	}

	if FailureKind(typeName) != kind {
		return errors.New("node error type does not match failure kind")
	}

	return nil
}

func (r *nodeRuntime) markHandledFailure() {
	if r == nil || r.execution == nil || r.execution.state == nil {
		return
	}

	r.execution.state.hasHandledFailure.Store(true)
}
