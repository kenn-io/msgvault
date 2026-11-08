package config

// GitHubConfig schedules syncing of the GitHub accounts added with
// add-github and chooses whose repositories they archive.
type GitHubConfig struct {
	// Enabled gates the daemon scheduler job.
	Enabled bool `toml:"enabled"`
	// Schedule is a 5-field cron expression; empty = not daemon-scheduled.
	Schedule string `toml:"schedule"`
	// IncludeOwners limits archiving to repositories owned by these
	// organizations or users (empty = every owner).
	IncludeOwners []string `toml:"include_owners"`
	// ExcludeOwners skips repositories owned by these organizations or
	// users; their threads are never fetched.
	ExcludeOwners []string `toml:"exclude_owners"`
}
