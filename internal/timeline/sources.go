package timeline

import (
	"context"
	"time"

	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/personagenda"
	"go.kenn.io/msgvault/internal/store"
)

// Sources separates archive readers from composition. Missing sources produce
// unavailable gaps. Source authorization always travels in the request context.
type Sources struct {
	Activity    ActivitySource
	Cold        ColdSource
	Calendar    CalendarSource
	Meetings    MeetingsSource
	Calls       CallsSource
	Loops       LoopsSource
	Inventory   InventorySource
	People      PeopleSource
	Notes       NotesSource
	Briefs      BriefsSource
	Agenda      AgendaSource
	Filters     store.ActivityFilters
	ArchiveUID  string
	Location    *time.Location
	ZoneDefault bool
	Clock       func() time.Time
}

type ActivitySource interface {
	ListActivityBurstsContext(ctx context.Context, q store.BurstQuery) (store.BurstPage, error)
	ListActivityMessagesContext(ctx context.Context, q store.MessageQuery) ([]store.ActivityMessage, error)
	MessageDisplayContext(ctx context.Context, ids []int64) (map[int64]store.MessageDisplay, error)
	LatestEligibleMessageContext(ctx context.Context, conversationID int64) (int64, error)
	MessageContactAggregateContext(ctx context.Context, q store.ContactQuery) (store.ContactAggregate, error)
	AttendedTranscriptIDsContext(ctx context.Context, q store.ContactQuery, limit int) ([]int64, bool, error)
	CadenceForPersonContext(ctx context.Context, personID int64, lastContact *time.Time, now time.Time) (string, *time.Time, error)
	ActivityProjectionStateContext(ctx context.Context) (store.ActivityProjectionState, error)
}
type ColdSource interface {
	ListColdActivityBurstsContext(ctx context.Context, q store.BurstQuery) (store.BurstPage, error)
	CountColdActivityMessagesContext(ctx context.Context, q store.BurstQuery) (int, error)
	ListColdActivityMessagesContext(ctx context.Context, q store.MessageQuery) ([]store.ActivityMessage, error)
}
type CalendarSource interface {
	ListCalendarOccurrencesContext(ctx context.Context, q store.CalendarOccurrenceQuery) (store.CalendarOccurrencePage, error)
	GetCalendarOccurrenceContext(ctx context.Context, ref string) (store.CalendarOccurrenceDetail, error)
}
type MeetingsSource interface {
	ResolveMeetingIdentitiesContext(ctx context.Context, q store.MeetingIdentityRequest) (store.MeetingIdentityResult, error)
	ParticipantsByEmailContext(ctx context.Context, emails []string) (map[string]store.AddressParticipant, error)
	MeetingActionItemCountsContext(ctx context.Context, ids []int64) (map[int64]int, error)
}
type CallsSource interface {
	ListLogicalCallsContext(ctx context.Context, q store.LogicalCallQuery) ([]store.LogicalCall, string, error)
	CallContactAggregateContext(ctx context.Context, q store.ContactQuery) (store.ContactAggregate, error)
}
type LoopsSource interface {
	List(ctx context.Context, opts loops.Options, now time.Time) (loops.Result, error)
}
type InventorySource interface {
	Build(ctx context.Context, now time.Time) (Freshness, error)
}
type PeopleSource interface {
	ResolvePersonReferenceContext(ctx context.Context, kind store.PersonRefKind, value string, allowedPersonIDs []int64, limit int) ([]store.PersonCandidate, error)
	PersonTiersContext(ctx context.Context, ids []int64, options store.PersonTierOptions) (map[int64]store.PersonTier, error)
}
type NotesSource interface {
	ListDailyNoteEntriesContext(ctx context.Context, date string, limit, offset int) ([]store.DailyNoteEntry, error)
}
type BriefsSource interface {
	GetPersonBriefContext(ctx context.Context, personID int64, maxLength int) (*store.PersonBrief, error)
}
type AgendaSource interface {
	List(ctx context.Context, personID int64) (personagenda.Result, error)
}
