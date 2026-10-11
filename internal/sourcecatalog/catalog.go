// Package sourcecatalog describes native archive transports and the completed
// capabilities their production adapters provide. Configuration and permission
// checks still determine whether a particular account can use each capability.
package sourcecatalog

import (
	"cmp"
	"slices"
)

// These identifiers are persisted archive namespaces, not display names.
const (
	Gmail             = "gmail"
	IMAP              = "imap"
	MicrosoftMail     = "msmail"
	Beeper            = "beeper"
	Slack             = "slack"
	Teams             = "teams"
	Discord           = "discord"
	Matrix            = "matrix"
	GoogleCalendar    = "gcal"
	Twilio            = "twilio"
	Granola           = "granola"
	Circleback        = "circleback"
	Plaud             = "plaud"
	NotionMeetings    = "notion_meetings"
	Muesli            = "muesli"
	SynctechSMS       = "synctech_sms"
	CardDAV           = "carddav"
	Twenty            = "twenty"
	EML               = "eml"
	Maildir           = "maildir"
	Mbox              = "mbox"
	AppleMail         = "apple-mail"
	PST               = "pst"
	GoogleVoice       = "google_voice"
	AppleMessages     = "apple_messages"
	IMazingCSV        = "imazing_csv"
	FacebookMessenger = "facebook_messenger"
	WhatsApp          = "whatsapp"
	Slackdump         = "slackdump"
	MeetingImport     = "meeting_import"
)

type Descriptor struct {
	Type, Transport                                                     string
	LiveMessages, Imports, Calendars, Meetings, Reactions, Drafts, Send bool
	Limitations                                                         []string
}

type registration struct {
	descriptor       Descriptor
	commands         []string
	scheduledAccount bool
}

// Capabilities become true only when the corresponding native producer/read
// boundary is implemented. The registry alone never enables an account.
var registrations = []registration{
	{Descriptor{Type: Gmail, Transport: "history_poll", LiveMessages: true, Drafts: true}, []string{"sync", "sync-full"}, true},
	{Descriptor{Type: IMAP, Transport: "mailbox_poll", LiveMessages: true, Drafts: true}, []string{"sync", "sync-full"}, true},
	{Descriptor{Type: MicrosoftMail, LiveMessages: true, Transport: "folder_delta_poll"}, []string{"sync"}, true},
	{Descriptor{Type: Beeper, LiveMessages: true, Transport: "desktop_poll", Drafts: true}, []string{"sync-beeper"}, false},
	{Descriptor{Type: Slack, LiveMessages: true, Transport: "api_poll", Drafts: true}, []string{"sync-slack"}, false},
	{Descriptor{Type: Teams, LiveMessages: true, Transport: "api_poll", Drafts: true}, []string{"sync-teams"}, true},
	{Descriptor{Type: Discord, LiveMessages: true, Transport: "bot_poll", Drafts: true}, []string{"sync-discord"}, true},
	{Descriptor{Type: Matrix, LiveMessages: true, Transport: "sync_poll", Limitations: []string{"encrypted_messages_unavailable"}}, []string{"sync-matrix"}, false},
	{Descriptor{Type: Twilio, Transport: "api_poll", Limitations: []string{"events_producer_unavailable"}}, []string{"sync-twilio"}, false},
	{Descriptor{Type: GoogleCalendar, Transport: "calendar_delta_poll", Calendars: true}, []string{"sync-calendar"}, false},
	{Descriptor{Type: Granola, Transport: "api_poll"}, []string{"sync-granola"}, false},
	{Descriptor{Type: Circleback, Transport: "mcp_poll"}, []string{"sync-circleback"}, false},
	{Descriptor{Type: Plaud, Transport: "mcp_poll"}, []string{"sync-plaud"}, false},
	{Descriptor{Type: NotionMeetings, Transport: "api_poll"}, []string{"sync-notion-meetings"}, false},
	{Descriptor{Type: Muesli, Transport: "local_snapshot_poll"}, []string{"sync-muesli"}, false},
	{Descriptor{Type: SynctechSMS, Transport: "backup_poll", Imports: true, Limitations: []string{"backup_import_transport"}}, []string{"sync-synctech-sms", "import-synctech-sms"}, false},
	{Descriptor{Type: Twenty, Transport: "graphql_poll", Limitations: []string{"events_producer_unavailable"}}, []string{"sync-twenty"}, false},
	{Descriptor{Type: CardDAV, Transport: "dav_poll", Limitations: []string{"contacts_only"}}, []string{"sync-carddav"}, false},
	{importDescriptor(EML), []string{"import-eml"}, false},
	{importDescriptor(Maildir), []string{"import-maildir"}, false},
	{importDescriptor(Mbox), []string{"import-mbox"}, false},
	{importDescriptor(AppleMail), []string{"import-emlx"}, false},
	{importDescriptor(PST), []string{"import-pst"}, false},
	{importDescriptor(GoogleVoice), []string{"import-gvoice"}, false},
	{importDescriptor(AppleMessages), []string{"import-imessage"}, false},
	{importDescriptor(IMazingCSV), []string{"import-imazing-csv"}, false},
	{importDescriptor(FacebookMessenger), []string{"import-messenger"}, false},
	{importDescriptor(WhatsApp), []string{"import-whatsapp"}, false},
	{Descriptor{Type: Slackdump, Transport: "import", Imports: true, Drafts: true, Limitations: []string{"offline_import_only"}}, []string{"import-slackdump"}, false},
	// The shared meeting importer is a native library boundary, not a CLI
	// command or a live transport.
	{importDescriptor(MeetingImport), nil, false},
}

func clone(d Descriptor) Descriptor {
	d.Limitations = slices.Clone(d.Limitations)
	return d
}

func List() []Descriptor {
	result := make([]Descriptor, 0, len(registrations))
	for _, registration := range registrations {
		result = append(result, clone(registration.descriptor))
	}
	slices.SortFunc(result, func(a, b Descriptor) int {
		return cmp.Compare(a.Type, b.Type)
	})
	return result
}

func Lookup(sourceType string) (Descriptor, bool) {
	for _, registration := range registrations {
		if registration.descriptor.Type == sourceType {
			return clone(registration.descriptor), true
		}
	}
	return Descriptor{}, false
}

func importDescriptor(sourceType string) Descriptor {
	return Descriptor{Type: sourceType, Transport: "import", Imports: true, Limitations: []string{"offline_import_only"}}
}

// Imported describes explicit importer provenance, including custom type
// names. It deliberately does not look up a provider with the same name.
func Imported(sourceType string) Descriptor { return importDescriptor(sourceType) }

func ForCommand(command string) []Descriptor {
	result := make([]Descriptor, 0)
	for _, registration := range registrations {
		if slices.Contains(registration.commands, command) {
			result = append(result, clone(registration.descriptor))
		}
	}
	return result
}

// Commands identifies the native registrations that own a source namespace.
// An empty result for a known type means a library-only importer.
func Commands(sourceType string) []string {
	for _, registration := range registrations {
		if registration.descriptor.Type == sourceType {
			return slices.Clone(registration.commands)
		}
	}
	return nil
}

// ScheduledAccountSync covers the shared account scheduler. Other native
// connectors have their own configured scheduler jobs and are not mail jobs.
func ScheduledAccountSync(sourceType string) bool {
	for _, registration := range registrations {
		if registration.descriptor.Type == sourceType {
			return registration.scheduledAccount
		}
	}
	return false
}
