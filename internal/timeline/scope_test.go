package timeline

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/personagenda"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
)

func TestDelegatedResponseIgnoresHiddenSourceAndNativeCalls(t *testing.T) {
	requirements := require.New(t)
	f := newTimelineFixture(t)
	f.savePerson()
	id := f.add(f.now, false)
	f.calendar("event", singleDayEvent)
	calendar, err := f.Store.GetOrCreateSource("gcal", "calendar@example.com")
	requirements.NoError(err)
	hidden, err := f.Store.GetOrCreateSource("gmail", "hidden@example.com")
	requirements.NoError(err)
	hiddenConv, err := f.Store.EnsureConversation(hidden.ID, "hidden", "Hidden")
	requirements.NoError(err)
	baselineCall := f.call(f.now.Add(-3 * time.Hour))
	callDisplay, err := f.Store.MessageDisplayContext(t.Context(), []int64{baselineCall})
	requirements.NoError(err)
	f.project()
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{f.Source.ID, calendar.ID, callDisplay[baselineCall].SourceID})
	requests := []Request{dayRequest(), anchorRequest(messageRef(id)), anchorRequest(fmt.Sprintf("conv:%d", f.ConvID))}
	for _, req := range append([]Request(nil), requests...) {
		req.IncludeNoise = true
		requests = append(requests, req)
	}
	before := [][]byte{}
	for i := range requests {
		requests[i].Scope = Scope{OpenTools: []string{"get_message"}}
		src := f.src
		if requests[i].Around != "" {
			src = snapshotAnchorBurstReads(ctx, t, src, requests[i], id)
		}
		got, e := Build(ctx, src, requests[i])
		requirements.NoError(e)
		raw, e := json.Marshal(got, json.Deterministic(true))
		requirements.NoError(e)
		before = append(before, raw)
	}
	hiddenID := f.add(f.now.Add(-time.Hour), false, func(m *store.Message) { m.SourceID = hidden.ID; m.ConversationID = hiddenConv })
	f.transcript("hidden-linked", "planning@example.com", f.now.Add(-2*time.Hour))
	nativeCall := f.call(f.now.Add(-time.Hour))
	projector, err := activity.NewProjector(f.Store, activity.Options{Timezone: "UTC"})
	requirements.NoError(err)
	_, err = projector.ProjectMessages(t.Context(), []int64{hiddenID})
	requirements.NoError(err)
	// A second hidden message intentionally remains queued.
	f.add(f.now, false, func(m *store.Message) { m.SourceID = hidden.ID; m.ConversationID = hiddenConv })
	var queuedCall bool
	err = f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT EXISTS(SELECT 1 FROM activity_projection_queue WHERE message_id=? AND revision>processed_revision)`), nativeCall).Scan(&queuedCall)
	requirements.NoError(err)
	requirements.True(queuedCall, "the native call mutation must remain unprojected")
	for _, phase := range []string{"unprojected", "projected"} {
		if phase == "projected" {
			f.project()
		}
		for i, req := range requests {
			src := f.src
			if req.Around != "" {
				// Recapture after each hidden/native mutation and projection
				// phase; a baseline snapshot would erase the scope regression.
				src = snapshotAnchorBurstReads(ctx, t, src, req, id)
			}
			got, e := Build(ctx, src, req)
			requirements.NoError(e)
			raw, e := json.Marshal(got, json.Deterministic(true))
			requirements.NoError(e)
			assert.Equal(t, string(before[i]), string(raw), "phase %s mode %s", phase, got.Mode)
		}
	}
}

func TestDelegatedCalendarIgnoresHiddenAccountIdentity(t *testing.T) {
	f := newTimelineFixture(t)
	f.calendar("event", singleDayEvent)
	calendar, err := f.Store.GetOrCreateSource("gcal", "calendar@example.com")
	require.NoError(t, err)
	hidden, err := f.Store.GetOrCreateSource("gmail", "hidden@example.com")
	require.NoError(t, err)
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{calendar.ID})
	req := dayRequest()
	req.Scope.Owner = false
	before, err := Build(ctx, f.src, req)
	require.NoError(t, err)
	entries := allDayEntries(before)
	require.Len(t, entries, 1)
	require.NotNil(t, entries[0].Meeting)
	require.Len(t, entries[0].Meeting.Attendees, 1)
	assert.Equal(t, "Alice", entries[0].Meeting.Attendees[0].Name)

	require.NoError(t, f.Store.AddAccountIdentity(hidden.ID, "alice@example.com", "manual"))
	after, err := Build(ctx, f.src, req)
	require.NoError(t, err)
	beforeJSON, err := json.Marshal(before, json.Deterministic(true))
	require.NoError(t, err)
	afterJSON, err := json.Marshal(after, json.Deterministic(true))
	require.NoError(t, err)
	assert.Equal(t, string(beforeJSON), string(afterJSON))
}

type deniedCalls struct{ hits *atomic.Int64 }

func (s deniedCalls) ListLogicalCallsContext(context.Context, store.LogicalCallQuery) ([]store.LogicalCall, string, error) {
	s.hits.Add(1)
	return nil, "", errors.New("owner-only calls queried")
}
func (s deniedCalls) CallContactAggregateContext(context.Context, store.ContactQuery) (store.ContactAggregate, error) {
	s.hits.Add(1)
	return store.ContactAggregate{}, errors.New("owner-only calls queried")
}

type deniedLoops struct{ hits *atomic.Int64 }

func (s deniedLoops) List(context.Context, loops.Options, time.Time) (loops.Result, error) {
	s.hits.Add(1)
	return loops.Result{}, errors.New("owner-only loops queried")
}

type deniedNotes struct{ hits *atomic.Int64 }

func (s deniedNotes) ListDailyNoteEntriesContext(context.Context, string, int, int) ([]store.DailyNoteEntry, error) {
	s.hits.Add(1)
	return nil, errors.New("owner-only notes queried")
}

type deniedBriefs struct{ hits *atomic.Int64 }

func (s deniedBriefs) GetPersonBriefContext(context.Context, int64, int) (*store.PersonBrief, error) {
	s.hits.Add(1)
	return nil, errors.New("owner-only brief queried")
}

type deniedAgenda struct{ hits *atomic.Int64 }

func (s deniedAgenda) List(context.Context, int64) (personagenda.Result, error) {
	s.hits.Add(1)
	return personagenda.Result{}, errors.New("owner-only agenda queried")
}

type deniedCadence struct {
	ActivitySource

	hits *atomic.Int64
}

func (s deniedCadence) CadenceForPersonContext(context.Context, int64, *time.Time, time.Time) (string, *time.Time, error) {
	s.hits.Add(1)
	return "", nil, errors.New("owner-only cadence queried")
}

type deniedSuggestions struct {
	MeetingsSource

	hits *atomic.Int64
}

func (s deniedSuggestions) ResolveMeetingIdentitiesContext(ctx context.Context, q store.MeetingIdentityRequest) (store.MeetingIdentityResult, error) {
	if q.IncludeSuggestions {
		s.hits.Add(1)
	}
	return s.MeetingsSource.ResolveMeetingIdentitiesContext(ctx, q)
}

func TestDelegatedOmitsOwnerOnlySectionsWithoutQuerying(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	f.savePerson()
	id := f.add(f.now, false)
	f.calendar("event", singleDayEvent)
	f.project()
	calendar, err := f.Store.GetOrCreateSource("gcal", "calendar@example.com")
	requirements.NoError(err)
	var hits atomic.Int64
	f.src.Calls = deniedCalls{&hits}
	f.src.Loops = deniedLoops{&hits}
	f.src.Notes = deniedNotes{&hits}
	f.src.Briefs = deniedBriefs{&hits}
	f.src.Agenda = deniedAgenda{&hits}
	f.src.Activity = deniedCadence{f.src.Activity, &hits}
	f.src.Meetings = deniedSuggestions{f.src.Meetings, &hits}
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{f.Source.ID, calendar.ID})
	for _, req := range []Request{dayRequest(), anchorRequest(messageRef(id))} {
		req.Scope = Scope{}
		got, e := Build(ctx, f.src, req)
		requirements.NoError(e)
		assertions.Nil(got.OpenLoops)
		if got.Person != nil {
			assertions.Nil(got.Person.Brief)
			assertions.Empty(got.Person.Agenda)
			assertions.Empty(got.Person.CadenceStatus)
		}
		for _, section := range []string{"calls", "loops", "meeting-link-suggestions", "brief", "agenda", "cadence", "notes"} {
			assertions.Contains(got.Gaps, Gap{Section: section, Code: "owner-only"})
		}
		for _, entry := range append(allDayEntries(got), got.History...) {
			assertions.NotEqual("call", entry.Kind)
			assertions.NotEqual("note", entry.Kind)
			if entry.Meeting != nil {
				assertions.Empty(entry.Meeting.PossiblyRelated)
			}
		}
	}
	assertions.Zero(hits.Load(), "an owner-only source was queried")
}
func TestDelegatedOpenTargetsOnlyAdmittedTools(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	f.add(f.now, false)
	f.calendar("event", singleDayEvent)
	f.transcript("unlinked", "", f.now.Add(-time.Hour))
	f.project()
	calendar, err := f.Store.GetOrCreateSource("gcal", "calendar@example.com")
	requirements.NoError(err)
	transcripts, err := f.Store.GetOrCreateSource("meeting_import", "notes@example.com")
	requirements.NoError(err)
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{f.Source.ID, calendar.ID, transcripts.ID})
	req := dayRequest()
	req.Scope = Scope{OpenTools: []string{"get_message"}}
	got, err := Build(ctx, f.src, req)
	requirements.NoError(err)
	requirements.GreaterOrEqual(len(allDayEntries(got)), 3)
	for _, e := range allDayEntries(got) {
		requirements.NotEmpty(e.Open)
		for _, open := range e.Open {
			assertions.Equal("get_message", open.Tool)
		}
	}
	req.Scope.OpenTools = []string{}
	got, err = Build(ctx, f.src, req)
	requirements.NoError(err)
	for _, e := range allDayEntries(got) {
		assertions.Empty(e.Open)
	}
}
func TestDelegatedRequiresExplicitNonemptySourceScope(t *testing.T) {
	f := newTimelineFixture(t)
	p := f.savePerson()
	id := f.add(f.now, false)
	f.project()
	for _, ctx := range []context.Context{t.Context(), sourcescope.WithSourceIDs(t.Context(), []int64{})} {
		for _, req := range []Request{dayRequest(), relationshipRequest(fmt.Sprintf("person:%d", p)), anchorRequest(messageRef(id))} {
			req.Scope = Scope{}
			_, err := Build(ctx, f.src, req)
			assert.ErrorIs(t, err, ErrInvalidRequest)
		}
	}
}

func TestDelegatedNativeCallAnchorsAreUnavailable(t *testing.T) {
	f := newTimelineFixture(t)
	id := f.call(f.now.Add(-time.Hour))
	f.project()
	display, err := f.Store.MessageDisplayContext(t.Context(), []int64{id})
	require.NoError(t, err)
	d := display[id]
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{d.SourceID})
	for _, around := range []string{messageRef(id), fmt.Sprintf("conv:%d", d.ConversationID), fmt.Sprintf("burst:%d:%d:2026-10-22", d.SourceID, d.ConversationID)} {
		req := anchorRequest(around)
		req.Scope = Scope{}
		_, err := Build(ctx, f.src, req)
		assert.ErrorIs(t, err, ErrUnknownReference, "anchor %s", around)
	}
}

func TestDelegatedConversationAnchorSkipsNewerNativeCall(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	call := f.call(f.now.Add(-time.Hour))
	display, err := f.Store.MessageDisplayContext(t.Context(), []int64{call})
	requirements.NoError(err)
	d := display[call]
	id := f.add(f.now.Add(-2*time.Hour), false, func(m *store.Message) { m.SourceID = d.SourceID; m.ConversationID = d.ConversationID })
	f.project()
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{d.SourceID})
	req := anchorRequest(fmt.Sprintf("conv:%d", d.ConversationID))
	req.IncludeNoise = true
	req.Scope = Scope{}
	req.Conversation = true
	got, err := Build(ctx, f.src, req)
	requirements.NoError(err)
	requirements.Len(got.History, 1)
	assertions.Equal(messageRef(id), got.History[0].Ref)
	assertions.True(got.History[0].Anchor)
	assertions.NotEqual(messageRef(call), got.History[0].Ref)
}

func TestDelegatedColdActivityOmitsNativeCalls(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	id := f.call(f.now)
	display, err := f.Store.MessageDisplayContext(t.Context(), []int64{id})
	requirements.NoError(err)
	d := display[id]
	// Exercise the cold reader against an archived call in a cold conversation.
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO conversation_policy(conversation_id,effective_tier) VALUES(?,'cold') ON CONFLICT(conversation_id) DO UPDATE SET effective_tier='cold'`), d.ConversationID)
	requirements.NoError(err)
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{d.SourceID})
	for _, includeNoise := range []bool{false, true} {
		req := dayRequest()
		req.Scope = Scope{}
		req.IncludeCold = true
		req.IncludeNoise = includeNoise
		got, err := Build(ctx, f.src, req)
		requirements.NoError(err)
		assertions.Empty(allDayEntries(got))
		assertions.Zero(got.Omitted.Noise)
		assertions.Zero(got.Omitted.Cold)
	}
}
