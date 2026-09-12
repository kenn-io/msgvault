package apiprotocol

const (
	ClientClassHeader       = "X-Msgvault-Client"
	ClientClassCLI          = "cli"
	DeduplicatePlanProtocol = "explicit-backfill-v1"
	// DaemonRuntimeTokenHeader is an HTTP header name, not a credential.
	// #nosec G101
	DaemonRuntimeTokenHeader = "X-Msgvault-Daemon-Token"
	// AgentTokenHeader carries a restricted agent grant secret. Header name, not a credential.
	// #nosec G101
	AgentTokenHeader = "X-Msgvault-Agent-Token"
)
