package clirun

// EnvIMAPPassword names the env var used to pass an IMAP password to a daemon-owned CLI subprocess.
const EnvIMAPPassword = "MSGVAULT_IMAP_PASSWORD" // #nosec G101 -- environment variable name, not a credential value

// EnvRemoteDeleteOptIn names the env var that opts into executing staged remote deletions.
const EnvRemoteDeleteOptIn = "MSGVAULT_ENABLE_REMOTE_DELETE"

func EnvAllowed(name string) bool {
	switch name {
	case EnvIMAPPassword, EnvRemoteDeleteOptIn:
		return true
	default:
		return false
	}
}
