package freshness

import "go.kenn.io/msgvault/internal/store"

// EmailSourceTypes is the frozen email inventory used by derived person
// context. Treat these source sets as read-only; unknown response sources are
// included separately so their freshness is still visible.
var EmailSourceTypes = []string{"gmail", "imap", "o365", "msmail", "hey", "apple-mail", "mbox", "pst", "eml", "maildir", "google-groups"}

// ChatSourceTypes follows the archive's single authenticated-sender registry.
// Imported Messenger conversations already belong to that registry.
var ChatSourceTypes = store.AuthenticatedChatSourceTypes()
