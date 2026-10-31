// Package cligrammar separates public command paths from stable daemon wire names.
// Wire mappings outlive the hidden CLI compatibility commands.
package cligrammar

import (
	"slices"
	"strings"
)

// Move describes a command whose public path changed. Legacy is also its stable
// daemon operation identifier. Existing flags and argument suffixes are unchanged.
type Move struct{ Legacy, Canonical string }

var moves = []Move{
	{"add-account", "source gmail add"},
	{"add-beeper", "source beeper add"},
	{"add-calendar", "source calendar add"},
	{"add-carddav", "source carddav add"},
	{"add-chatwoot", "source chatwoot add"},
	{"add-circleback", "source circleback add"},
	{"add-discord", "source discord add"},
	{"add-granola", "source granola add"},
	{"add-imap", "source imap add"},
	{"add-inline", "source inline add"},
	{"add-matrix", "source matrix add"},
	{"add-muesli", "source muesli add"},
	{"add-notion-meetings", "source notion-meetings add"},
	{"add-o365", "source o365 add"},
	{"add-omi", "source omi add"},
	{"add-plaud", "source plaud add"},
	{"add-slack", "source slack add"},
	{"add-synctech-sms-drive", "source synctech-sms add"},
	{"add-teams", "source teams add"},
	{"add-twenty", "source twenty add"},
	{"add-twilio", "source twilio add"},
	{"archive-remote-images", "attachment archive-remote-images"},
	{"backfill-beeper-media", "source beeper backfill-media"},
	{"backfill-discord-media", "source discord backfill-media"},
	{"backfill-inline-media", "source inline backfill-media"},
	{"backfill-slack-media", "source slack backfill-media"},
	{"backfill-teams-media", "source teams backfill-media"},
	{"build-cache", "cache build"},
	{"cache-stats", "cache stats"},
	{"cancel-deletion", "deletion cancel"},
	{"create-subset", "export subset"},
	{"delete-deduped", "deletion purge-deduped"},
	{"delete-staged", "deletion execute"},
	{"draft-compose", "draft compose"},
	{"draft-delete", "draft delete"},
	{"draft-edit", "draft edit"},
	{"draft-forward", "draft forward"},
	{"draft-get", "draft get"},
	{"draft-recover", "draft recover"},
	{"draft-reply", "draft reply"},
	{"draft-send-as", "draft send-as"},
	{"export-attachment", "export attachment"},
	{"export-attachments", "export attachments"},
	{"export-discord", "export discord"},
	{"export-eml", "export eml"},
	{"export-messages", "export messages"},
	{"export-token", "account export-token"},
	{"find-chat", "chat find"},
	{"import-eml", "import eml"},
	{"import-emlx", "import emlx"},
	{"import-gvoice", "import gvoice"},
	{"import-imazing-csv", "import imazing-csv"},
	{"import-imessage", "import imessage"},
	{"import-maildir", "import maildir"},
	{"import-mbox", "import mbox"},
	{"import-messenger", "import messenger"},
	{"import-pst", "import pst"},
	{"import-slackdump", "import slackdump"},
	{"import-synctech-sms", "import synctech-sms"},
	{"import-whatsapp", "import whatsapp"},
	{"init-db", "database init"},
	{"list-accounts", "account list"},
	{"list-deletions", "deletion list"},
	{"list-domains", "domain list"},
	{"list-folders", "source imap folders"},
	{"list-labels", "label list"},
	{"list-senders", "sender list"},
	{"merge-account", "account merge"},
	{"message-tags", "message tags"},
	{"migrate", "database migrate"},
	{"muesli-hook", "source muesli hook"},
	{"pack-attachments", "attachment pack"},
	{"purge-excluded-media", "attachment purge-excluded"},
	{"rebuild-fts", "fts rebuild"},
	{"remove-account", "account remove"},
	{"repack-attachments", "attachment repack"},
	{"repair-dates", "repair dates"},
	{"repair-derived", "repair derived"},
	{"repair-encoding", "repair encoding"},
	{"repair-identity", "repair identity"},
	{"repair-labels", "repair labels"},
	{"repair-list-ids", "repair list-ids"},
	{"repair-message", "repair message"},
	{"repair-senders", "repair senders"},
	{"schema-version", "database version"},
	{"show-deletion", "deletion show"},
	{"show-message", "show"},
	{"show-thread", "thread show"},
	{"stage-delete", "deletion stage"},
	{"sync-beeper", "source beeper sync"},
	{"sync-calendar", "source calendar sync"},
	{"sync-carddav", "source carddav sync"},
	{"sync-chatwoot", "source chatwoot sync"},
	{"sync-circleback", "source circleback sync"},
	{"sync-discord", "source discord sync"},
	{"sync-full", "sync full"},
	{"sync-granola", "source granola sync"},
	{"sync-inline", "source inline sync"},
	{"sync-matrix", "source matrix sync"},
	{"sync-muesli", "source muesli sync"},
	{"sync-notion-meetings", "source notion-meetings sync"},
	{"sync-omi", "source omi sync"},
	{"sync-plaud", "source plaud sync"},
	{"sync-slack", "source slack sync"},
	{"sync-synctech-sms", "source synctech-sms sync"},
	{"sync-teams", "source teams sync"},
	{"sync-twenty", "source twenty sync"},
	{"sync-twilio", "source twilio sync"},
	{"unpack-attachments", "attachment unpack"},
	{"update-account", "account update"},
}

// Moves returns the structural renames, independent of the caller's Cobra tree.
func Moves() []Move { return slices.Clone(moves) }

type pathMove struct{ legacy, canonical []string }

var paths = func() []pathMove {
	result := make([]pathMove, 0, len(moves))
	for _, move := range moves {
		result = append(result, pathMove{strings.Fields(move.Legacy), strings.Fields(move.Canonical)})
	}
	return result
}()

// Canonical converts stable operation names to executable public command paths.
// Only complete leading command tokens match; argument and flag data is untouched.
func Canonical(args []string) []string {
	// Preserve the stable daemon operation identifier for existing clients.
	// The CLI no longer registers this name as a command.
	if len(args) > 0 && args[0] == "build-embeddings" {
		return append([]string{"embeddings", "build"}, args[1:]...)
	}
	return translate(args, false)
}

// Legacy converts public command paths to stable wire/classification names.
// New clients use these identifiers to remain compatible with older daemons.
func Legacy(args []string) []string { return translate(args, true) }

func translate(args []string, legacy bool) []string {
	var replacement []string
	matched := 0
	for _, path := range paths {
		from, to := path.legacy, path.canonical
		if legacy {
			from, to = to, from
		}
		if len(from) > matched && len(args) >= len(from) && slices.Equal(args[:len(from)], from) {
			matched, replacement = len(from), to
		}
	}
	if matched == 0 {
		return slices.Clone(args)
	}
	result := make([]string, 0, len(replacement)+len(args)-matched)
	result = append(result, replacement...)
	return append(result, args[matched:]...)
}
