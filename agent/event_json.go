package agent

// MarshalJSON prevents the process-local event union from being mistaken for
// a stable wire protocol.
func (Event) MarshalJSON() ([]byte, error) {
	return nil, ErrEventWireFormat
}

// UnmarshalJSON prevents decoding an unspecified wire representation into an
// Event. Use an application-owned, versioned projection instead.
func (*Event) UnmarshalJSON([]byte) error {
	return ErrEventWireFormat
}
