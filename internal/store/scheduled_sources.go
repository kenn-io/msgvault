package store

// Provider identifiers shared by scheduled-source selection and sync dispatch.
const (
	SourceTypeGmail   = "gmail"
	SourceTypeIMAP    = "imap"
	SourceTypeMSMail  = "msmail"
	SourceTypeTeams   = "teams"
	SourceTypeDiscord = "discord"
)

// SelectScheduledSyncSources keeps one active source per provider in sync order.
func SelectScheduledSyncSources(rows []*Source, identifier string) []*Source {
	// Collect first occurrence of each syncable type.
	seen := make(map[string]*Source, 4)
	for _, src := range rows {
		if src.MergedIntoSourceID != 0 {
			continue
		}
		switch src.SourceType {
		case SourceTypeGmail, SourceTypeIMAP, SourceTypeMSMail, SourceTypeTeams:
			if _, dup := seen[src.SourceType]; !dup {
				seen[src.SourceType] = src
			}
		case SourceTypeDiscord:
			// Guild display names are not stable or unique. Scheduled Discord
			// jobs must use the exact guild snowflake as their key.
			if src.Identifier == identifier {
				seen[src.SourceType] = src
			}
		}
	}

	// Return in stable order: gmail, imap, msmail, teams, discord.
	var result []*Source
	for _, t := range []string{SourceTypeGmail, SourceTypeIMAP, SourceTypeMSMail, SourceTypeTeams, SourceTypeDiscord} {
		if src, ok := seen[t]; ok {
			result = append(result, src)
		}
	}
	return result
}
