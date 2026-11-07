package config

// FeedsConfig schedules fetching of the RSS, Atom and JSON feeds followed
// with add-feed. The feeds themselves live in the archive, not here.
type FeedsConfig struct {
	// Enabled gates the daemon scheduler job.
	Enabled bool `toml:"enabled"`
	// Schedule is a 5-field cron expression; empty = not daemon-scheduled.
	Schedule string `toml:"schedule"`
}
