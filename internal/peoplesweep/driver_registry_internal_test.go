package peoplesweep

// NewTestDriverRegistry constructs a registry with one controlled driver for
// package consumers' runner tests. It is compiled only into test binaries.
func NewTestDriverRegistry(protocol Protocol, driver StructuredDriver) *DriverRegistry {
	return &DriverRegistry{drivers: map[Protocol]StructuredDriver{protocol: driver}}
}
