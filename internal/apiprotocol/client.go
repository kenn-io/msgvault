package apiprotocol

const (
	ClientClassHeader = "X-Msgvault-Client"
	ClientClassCLI    = "cli"
	// DaemonRuntimeTokenHeader is an HTTP header name, not a credential.
	// #nosec G101
	DaemonRuntimeTokenHeader = "X-Msgvault-Daemon-Token"
)
