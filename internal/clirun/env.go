package clirun

// EnvIMAPPassword names the env var used to pass an IMAP password to a daemon-owned CLI subprocess.
const EnvIMAPPassword = "MSGVAULT_IMAP_PASSWORD" // #nosec G101 -- environment variable name, not a credential value

// EnvBeeperToken names the env var used to pass a Beeper Desktop access token
// to a daemon-owned CLI subprocess.
const EnvBeeperToken = "MSGVAULT_BEEPER_TOKEN" // #nosec G101 -- environment variable name, not a credential value

// EnvDiscordToken names the env var used to pass a Discord bot token to a
// daemon-owned CLI subprocess.
const EnvDiscordToken = "MSGVAULT_DISCORD_TOKEN" // #nosec G101 -- environment variable name, not a credential value

// EnvSlackToken names the env var used to pass a Slack user token to a
// daemon-owned CLI subprocess.
const EnvSlackToken = "MSGVAULT_SLACK_TOKEN" // #nosec G101 -- environment variable name, not a credential value

// EnvRemoteDeleteOptIn names the env var that opts into executing staged remote deletions.
const EnvRemoteDeleteOptIn = "MSGVAULT_ENABLE_REMOTE_DELETE"

func EnvAllowed(name string) bool {
	switch name {
	case EnvIMAPPassword, EnvBeeperToken, EnvDiscordToken, EnvSlackToken, EnvRemoteDeleteOptIn:
		return true
	default:
		return false
	}
}
