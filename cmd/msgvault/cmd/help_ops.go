package cmd

import "github.com/spf13/cobra"

// applyOpsHelp describes groups synthesized by the public command grammar.
// Leaf guidance lives with each command's original constructor.
func applyOpsHelp(root *cobra.Command) {
	groups := map[string]struct{ long, example string }{
		"attachment": {
			"Maintain archived attachment storage. Pack combines loose blobs, unpack restores loose files with the daemon stopped, and repack reclaims dead bytes. purge-excluded previews policy exclusions with --dry-run before applying them. archive-remote-images separately requires consent to sender-controlled requests.",
			"  msgvault attachment pack\n  msgvault attachment purge-excluded --dry-run",
		},
		"cache": {
			"Build the derived Parquet analytics cache used for fast aggregate queries and the TUI. Inspect stats, build incrementally, or use --full-rebuild after archive repairs or local purges. Cache rebuilds do not change provider messages.",
			"  msgvault cache stats\n  msgvault cache build --full-rebuild",
		},
		"database": {
			"Initialize or inspect the main archive schema. version prints this binary's expected version; version --database reads the local archive without migrating it. migrate applies schema changes offline: stop the daemon and all other archive users and make a backup first.",
			"  msgvault database version\n  msgvault daemon stop\n  msgvault database migrate",
		},
		"deletion": {
			"Stage and review remote deletions before executing them. stage --dry-run previews a selection; stage records a batch, show reviews it, and execute --dry-run previews provider changes. The archive owner must opt in to remote deletion; agent grants cannot enable it. Trash is the default. Permanent deletion always requires typing delete, even with --yes. Cancel staged work with cancel. purge-deduped is a separate irreversible local purge.",
			"  msgvault deletion stage 'from:sender@example.com' --dry-run\n  msgvault deletion list\n  msgvault deletion show batch-example\n  msgvault deletion execute --dry-run",
		},
		"draft": {
			"Create and manage drafts for review. reply and forward use an internal archived message ID; compose starts a new email, native Beeper, or local chat draft. get returns the revision needed by edit, delete, and recover. Send drafts through the provider client; there is no draft send command. Provider support and source draft grants determine which operations are available.",
			"  msgvault draft reply 1234 --body 'Thanks; I will review it.' --json\n  msgvault draft get draft-example --json",
		},
		"fts": {
			"Maintain the local full-text search index over archived message text. Rebuild after an index repair or when indexed content needs refreshing. Rebuilding reads the archive and replaces derived search data; it does not contact message providers.",
			"  msgvault fts rebuild",
		},
		"message": {
			"Inspect or update provider tags for an archived message ID returned by search. tags without edit flags reads the current provider state; --add and --remove change native tags. Preview an edit using --dry-run.",
			"  msgvault message tags 1234 --json",
		},
		"repair": {
			"Repair archive evidence and derived data. dates, labels, list-ids, and senders report changes by default and require --apply to write. encoding, identity, and derived write immediately. message refreshes one Gmail snapshot from the provider; message --audit checks stored MIME without writing. Back up before repairs that write.",
			"  msgvault repair dates\n  msgvault repair labels --apply\n  msgvault repair message --audit --json",
		},
	}
	for name, help := range groups {
		if command := directCommand(root, name); command != nil {
			command.Long, command.Example = help.long, help.example
		}
	}
}
