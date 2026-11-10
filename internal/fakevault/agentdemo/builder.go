package agentdemo

import (
	"context"

	"go.kenn.io/msgvault/internal/store"
)

const (
	emailKind        = "email"
	slackSource      = "slack"
	ownerPhoneHandle = "owner_phone"
)

type builder struct {
	st                *store.Store
	attachmentsDir    string
	opts              Options
	m                 *Manifest
	automatedMessages []string
}
type chapter struct {
	name string
	run  func(context.Context, *builder) error
}

var chapters = []chapter{{"cast", cast}, {emailKind, email}, {"chats", chats}, {"voice", voice}}

func newBuilder(st *store.Store, dir string, opts Options) *builder {
	return &builder{st: st, attachmentsDir: dir, opts: opts, m: &Manifest{
		Scenario:        Scenario,
		ScenarioVersion: ScenarioVersion,
		Seed:            opts.Seed,
		AsOf:            opts.AsOf,
		Owner:           Owner{"Morgan Lee", "morgan@example.com", "+15550100100", "America/New_York"},
		Sources:         map[string]Source{},
		Persons:         map[string]Person{},
		Participants:    map[string]Participant{},
		Conversations:   map[string]Conversation{},
		Messages:        map[string]int64{},
		Attachments:     map[string]Attachment{},
		Meetings:        map[string]Meeting{},
		CalendarEvents:  map[string]int64{},
		Operations:      map[string]Operation{},
		Collections:     map[string]int64{},
		SavedViews:      map[string]int64{},
		Expect:          map[string]any{},
		Canaries:        []string{"CANARY-SUBJ-7f3a", "CANARY-BODY-7f3a", "canary-addr-7f3a@canary.example", "Canary Q. Person"},
	}}
}
func (b *builder) source(h string) int64      { return b.m.Sources[h].ID }
func (b *builder) participant(h string) int64 { return b.m.Participants[h].ID }
func (b *builder) message(h string) int64     { return b.m.Messages[h] }
