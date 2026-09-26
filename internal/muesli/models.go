// Package muesli archives meetings recorded by the Muesli macOS app
// (github.com/Muesli-HQ/muesli) by reading its local SQLite database
// read-only.
package muesli

const (
	SourceType = "muesli"
	RawFormat  = "muesli_json"
)

// Participant is one person Muesli attached to a meeting, either from the
// calendar event or picked manually from Contacts. Muesli's Contacts and
// calendar identifiers are deliberately not carried.
type Participant struct {
	Name   string
	Email  string // lowercased; empty when Muesli has none
	Source string // "calendar", "manual", or empty on old databases
	// Identifier is Muesli's participant_identifier ("email:…", "contact:…",
	// "calendar:…"). It stays in memory; raw evidence carries only a hash.
	Identifier string
	// ContactEmails and ContactPhones (E.164) come from the Mac's Contacts
	// card for this participant; Anchor links them. Resolution is "resolved"
	// or "carried_forward" when they are set.
	ContactEmails []string
	ContactPhones []string
	Anchor        string
	Resolution    string
	// SkippedPhones counts Contacts phones that could not become E.164.
	SkippedPhones int
}

// Meeting is one row of Muesli's meetings table. Text columns hold Muesli's
// values verbatim; columns an older database lacks are left empty.
type Meeting struct {
	ID               int64
	Title            string
	StartTime        string
	EndTime          string
	CreatedAt        string
	DurationSeconds  float64
	Status           string
	Source           string
	RawTranscript    string
	FormattedNotes   string
	ManualNotes      string
	WordCount        int64
	TemplateName     string
	TemplateKind     string
	CalendarEventID  string
	CalendarSource   string
	CalendarSeriesID string
	Folder           string // folder path such as "Clients/Acme"
	FollowUpToID     int64
	Deleted          bool
	Participants     []Participant // not suppressed, in Muesli's display order
	// ContactsState is how much of Contacts the sync could read.
	ContactsState ContactsState
}
