package store_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
)

func emptyEventBrief() json.RawMessage {
	return json.RawMessage(`{"last_meaningful_interaction":null,"highlights":[],"follow_ups":[],"appreciations":[],"uncertainties":[],"possible_attributes":[]}`)
}

func storeEventCandidate(t *testing.T, packet map[string]any, announcement, date, text, question string) json.RawMessage {
	t.Helper()
	var highlight map[string]any
	if context, ok := packet["brief_context"].(map[string]any); ok {
		if events, ok := context["events"].([]any); ok && len(events) > 0 {
			highlight, _ = events[0].(map[string]any)
		}
	}
	var resolutionID, resolutionQuote string
	seeds, ok := packet["seeds"].([]any)
	require.True(t, ok)
	for _, raw := range seeds {
		item, ok := raw.(map[string]any)
		require.True(t, ok)
		quote, _ := item["excerpt"].(string)
		id, _ := item["id"].(string)
		if strings.Contains(quote, announcement) && highlight == nil {
			highlight = map[string]any{
				"text": text, "speaker": "person", "evidence_ids": []string{id},
				"observed_at": nil, "confidence_basis_points": 900,
				"life_event": map[string]any{
					"date": date, "anchor_id": id, "quote": announcement,
					"question": question, "resolution": nil, "state": "upcoming",
				},
			}
		}
		if strings.Contains(quote, "I cancelled the interview.") {
			resolutionID, resolutionQuote = id, "I cancelled the interview."
		}
	}
	require.NotNil(t, highlight, "packet must carry the announcement or retained event")
	if resolutionID != "" {
		event, ok := highlight["life_event"].(map[string]any)
		require.True(t, ok)
		anchorID, ok := event["anchor_id"].(string)
		require.True(t, ok)
		event["resolution"] = map[string]any{"evidence_id": resolutionID, "quote": resolutionQuote}
		highlight["evidence_ids"] = []string{anchorID, resolutionID}
	}
	var output map[string]any
	require.NoError(t, json.Unmarshal(emptyEventBrief(), &output))
	output["highlights"] = []any{highlight}
	encoded, err := json.Marshal(output)
	require.NoError(t, err)
	return encoded
}

func newStoreEventFixture(t *testing.T, suffix string) (briefWorkerEndToEndFixture, *time.Time, int64) {
	t.Helper()
	f := newBriefWorkerEndToEndFixture(t, suffix)
	anchor := f.journal.insertMessage(t, "event-anchor-"+suffix, "chat", f.journal.aliceID,
		time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC))
	addSweepBody(t, f.journal, anchor, "I interview on September 2.")
	now := f.now
	f.worker.Clock = func() time.Time { return now }
	sequence := 0
	f.worker.NewID = func() string { sequence++; return fmt.Sprintf("event-%s-%d", suffix, sequence) }
	f.runner.candidate = func(packet map[string]any) json.RawMessage {
		return storeEventCandidate(t, packet, "I interview on September 2.", "2026-09-02", "their interview", "How did the interview go?")
	}
	return f, &now, anchor
}

func runStoreEvent(t *testing.T, f briefWorkerEndToEndFixture, mode peoplesweep.BriefMode) peoplesweep.RunResult {
	t.Helper()
	result, err := f.worker.Run(t.Context(), peoplesweep.RunRequest{
		Kind: peoplesweep.RunScheduled, Mode: peoplesweep.RunIncremental,
		PersonID: f.journal.alicePersonID, Limit: 1, Brief: mode,
	})
	require.NoError(t, err)
	return result
}

func TestPersonSweepEventLifecyclePersistsDateOnlyRefreshAndResolution(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, now, _ := newStoreEventFixture(t, "lifecycle")
	st, person := f.journal.store, f.journal.alicePersonID
	first := runStoreEvent(t, f, peoplesweep.BriefModeForce)
	require.Equal(1, first.PeopleSucceeded)
	upcoming, err := st.GetPersonBriefContext(t.Context(), person, 0)
	require.NoError(err)
	assert.Equal("v2", upcoming.ProgramVersion)
	assert.Contains(upcoming.RenderedText, "Upcoming:")
	assert.NotContains(upcoming.RenderedText, "How did the interview go?")
	var initialBoundary peoplesweep.BriefBoundary
	require.NoError(json.Unmarshal(upcoming.Boundary, &initialBoundary))
	require.Len(initialBoundary.EventMemory, 1)

	// The provider omits the event; Go carries it. Only the clock changes.
	f.runner.candidate = func(map[string]any) json.RawMessage { return emptyEventBrief() }
	*now = time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	due := runStoreEvent(t, f, peoplesweep.BriefModeAuto)
	require.Equal(1, due.PeopleSucceeded)
	current, err := st.GetPersonBriefContext(t.Context(), person, 0)
	require.NoError(err)
	assert.Equal(2, current.Version)
	assert.Contains(current.RenderedText, "How did the interview go?")
	var dueBoundary peoplesweep.BriefBoundary
	require.NoError(json.Unmarshal(current.Boundary, &dueBoundary))
	assert.Equal(initialBoundary.ThroughSequence, dueBoundary.ThroughSequence)
	assert.Nil(dueBoundary.NextEventAt)
	pointers, err := st.ListPersonBriefEvidenceContext(t.Context(), current.ID)
	require.NoError(err)
	require.Len(pointers, 1)
	assert.True(pointers[0].Supported)

	resolution := f.journal.insertMessage(t, "event-resolution", "chat", f.journal.aliceID, now.Add(time.Hour))
	addSweepBody(t, f.journal, resolution, "I cancelled the interview.")
	f.runner.candidate = func(packet map[string]any) json.RawMessage {
		return storeEventCandidate(t, packet, "I interview on September 2.", "2026-09-02", "their interview", "How did the interview go?")
	}
	*now = now.Add(2 * time.Hour)
	resolved := runStoreEvent(t, f, peoplesweep.BriefModeAuto)
	require.Equal(1, resolved.PeopleSucceeded)
	current, err = st.GetPersonBriefContext(t.Context(), person, 0)
	require.NoError(err)
	assert.Equal(3, current.Version)
	assert.Contains(current.RenderedText, "Resolved:")
	assert.NotContains(current.RenderedText, "How did the interview go?")
	pointers, err = st.ListPersonBriefEvidenceContext(t.Context(), current.ID)
	require.NoError(err)
	assert.Len(pointers, 2)

	f.runner.candidate = func(map[string]any) json.RawMessage { return emptyEventBrief() }
	runStoreEvent(t, f, peoplesweep.BriefModeForce)
	current, err = st.GetPersonBriefContext(t.Context(), person, 0)
	require.NoError(err)
	assert.Equal(4, current.Version)
	assert.Contains(current.RenderedText, "Resolved:")
	assert.NotContains(current.RenderedText, "How did the interview go?")
}

func TestPersonSweepEventRetiresCurrentParagraphOnceWhenAllTextIsGone(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, now, _ := newStoreEventFixture(t, "retire")
	st, person := f.journal.store, f.journal.alicePersonID
	runStoreEvent(t, f, peoplesweep.BriefModeForce)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM messages WHERE conversation_id = ?`), f.journal.conversationID)
	require.NoError(err)
	*now = now.Add(2 * time.Hour)
	runStoreEvent(t, f, peoplesweep.BriefModeAuto)
	_, err = st.GetPersonBriefContext(t.Context(), person, 0)
	require.ErrorIs(err, store.ErrPersonBriefNotFound)
	history, err := st.ListPersonBriefVersionsContext(t.Context(), person, 10)
	require.NoError(err)
	require.Len(history, 1)
	assert.Equal(store.PersonBriefStatusSuperseded, history[0].Status)
	assert.NotNil(history[0].SupersededAt)
	var boundary peoplesweep.BriefBoundary
	require.NoError(json.Unmarshal(history[0].Boundary, &boundary))
	assert.False(boundary.AdmissionThroughEventTime.IsZero())
	before := f.runner.briefRequests
	runStoreEvent(t, f, peoplesweep.BriefModeAuto)
	assert.Equal(before, f.runner.briefRequests, "empty retired history makes no provider call")
	newAnchor := f.journal.insertMessage(t, "event-after-retirement", "chat", f.journal.aliceID, now.Add(time.Hour))
	addSweepBody(t, f.journal, newAnchor, "I interview on September 2.")
	*now = now.Add(2 * time.Hour)
	runStoreEvent(t, f, peoplesweep.BriefModeAuto)
	current, err := st.GetPersonBriefContext(t.Context(), person, 0)
	require.NoError(err)
	assert.Equal(2, current.Version)
	assert.Contains(current.RenderedText, "Upcoming:")
	var refreshed peoplesweep.BriefBoundary
	require.NoError(json.Unmarshal(current.Boundary, &refreshed))
	assert.True(refreshed.AdmissionThroughEventTime.After(boundary.AdmissionThroughEventTime))
}

func TestPersonSweepEventAlignmentRaceKeepsExtractionAndRetriesBrief(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, _, anchor := newStoreEventFixture(t, "alignment")
	st, person := f.journal.store, f.journal.alicePersonID
	f.runner.afterBrief = func() {
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE message_bodies SET body_text = ? WHERE message_id = ?`),
			"The source was edited after the provider read it.", anchor)
		require.NoError(err)
	}
	result := runStoreEvent(t, f, peoplesweep.BriefModeForce)
	require.Equal(1, result.PeopleSucceeded)
	require.Len(result.People, 1)
	assert.Equal(peoplesweep.FailureInvalidOutput, result.People[0].BriefFailureClass)
	assert.Zero(result.People[0].BriefVersion)
	assert.NotEmpty(result.People[0].CursorAdvances)
	_, err := st.GetPersonBriefContext(t.Context(), person, 0)
	require.ErrorIs(err, store.ErrPersonBriefNotFound)
}

func TestPersonSweepEventRetirementRequiresLeaseAndExactVersion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, now, _ := newStoreEventFixture(t, "fence")
	st, person := f.journal.store, f.journal.alicePersonID
	runStoreEvent(t, f, peoplesweep.BriefModeForce)
	_, err := st.EnsurePersonSweepWork(t.Context(), person, true)
	require.NoError(err)
	lease, err := st.ClaimPersonSweep(t.Context(), peoplesweep.ClaimRequest{
		PersonID: person, WorkerID: "event-retirement-test", LeaseDuration: time.Minute,
	})
	require.NoError(err)
	require.NotNil(lease)
	require.Error(st.RetirePersonSweepBrief(t.Context(), *lease, 2, *now))
	stale := *lease
	stale.Fence++
	require.ErrorIs(st.RetirePersonSweepBrief(t.Context(), stale, 1, *now), peoplesweep.ErrLeaseLost)
	current, err := st.GetPersonBriefContext(t.Context(), person, 0)
	require.NoError(err)
	assert.Equal(1, current.Version)
	require.NoError(st.RetirePersonSweepBrief(t.Context(), *lease, 1, *now))
	_, err = st.GetPersonBriefContext(t.Context(), person, 0)
	require.ErrorIs(err, store.ErrPersonBriefNotFound)
}

func TestPersonSweepEventBootstrapsLegacyBriefWithoutActivityBeyondOverlap(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, now, _ := newStoreEventFixture(t, "bootstrap")
	st, person := f.journal.store, f.journal.alicePersonID
	// The announcement is older than ordinary overlap but inside the bounded
	// initial window. A v1 brief has no typed event state or date trigger.
	for i := range f.config.Brief.OverlapItems + 2 {
		id := f.journal.insertMessage(t, fmt.Sprintf("bootstrap-newer-%d", i), "chat", f.journal.aliceID,
			time.Date(2026, 9, 1, 11, i, 0, 0, time.UTC))
		addSweepBody(t, f.journal, id, "Just a routine update.")
	}
	f.runner.candidate = nil
	runStoreEvent(t, f, peoplesweep.BriefModeForce)
	current, err := st.GetPersonBriefContext(t.Context(), person, 0)
	require.NoError(err)
	var boundary peoplesweep.BriefBoundary
	require.NoError(json.Unmarshal(current.Boundary, &boundary))
	boundary.EventMemoryVersion = 0
	boundary.EventMemory = nil
	boundary.NextEventAt = nil
	boundary.AdmissionThroughEventTime = time.Time{}
	legacyBoundary, err := json.Marshal(boundary)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_briefs
		SET program_version = 'v1', boundary_json = ? WHERE id = ?`), string(legacyBoundary), current.ID)
	require.NoError(err)
	f.runner.candidate = func(packet map[string]any) json.RawMessage {
		return storeEventCandidate(t, packet, "I interview on September 2.", "2026-09-02", "their interview", "How did the interview go?")
	}
	*now = now.Add(time.Minute)
	result := runStoreEvent(t, f, peoplesweep.BriefModeAuto)
	require.Equal(1, result.PeopleSucceeded)
	current, err = st.GetPersonBriefContext(t.Context(), person, 0)
	require.NoError(err)
	assert.Equal(2, current.Version)
	assert.Equal("v2", current.ProgramVersion)
	require.NoError(json.Unmarshal(current.Boundary, &boundary))
	require.Len(boundary.EventMemory, 1)
	assert.Contains(current.RenderedText, "Upcoming:")
	before := f.runner.briefRequests
	runStoreEvent(t, f, peoplesweep.BriefModeAuto)
	assert.Equal(before, f.runner.briefRequests, "committed v2 bootstrap does not repeat")
}

func TestPersonSweepEventPolicyRefusalRetiresCurrentOnceWithoutInference(t *testing.T) {
	for _, policy := range []string{"no_sensitive", "no_brief_lane"} {
		t.Run(policy, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, now, _ := newStoreEventFixture(t, policy)
			st, person := f.journal.store, f.journal.alicePersonID
			runStoreEvent(t, f, peoplesweep.BriefModeForce)
			provider := f.worker.Config.Providers["default"]
			if policy == "no_sensitive" {
				provider.AllowSensitive = false
			} else {
				provider.AllowedSources = []peoplesweep.SourceClass{peoplesweep.SourceMeetingText}
			}
			f.worker.Config.Providers["default"] = provider
			profile, err := f.worker.Config.Profile()
			require.NoError(err)
			_, err = st.EnsurePersonInferenceProfile(t.Context(), profile)
			require.NoError(err)
			_, _, err = st.GrantPersonInferenceConsent(t.Context(), profile.Fingerprint, "test-owner")
			require.NoError(err)
			sweepCursorsToHighWater(t, st, person, profile, *now)
			beforeBrief, beforeExtraction := f.runner.briefRequests, f.runner.extractionRequests
			*now = now.Add(time.Minute)
			runStoreEvent(t, f, peoplesweep.BriefModeAuto)
			_, err = st.GetPersonBriefContext(t.Context(), person, 0)
			require.ErrorIs(err, store.ErrPersonBriefNotFound)
			history, err := st.ListPersonBriefVersionsContext(t.Context(), person, 10)
			require.NoError(err)
			require.Len(history, 1)
			assert.Equal(store.PersonBriefStatusSuperseded, history[0].Status)
			require.NotNil(history[0].SupersededAt)
			retiredAt := *history[0].SupersededAt
			*now = now.Add(time.Minute)
			runStoreEvent(t, f, peoplesweep.BriefModeAuto)
			history, err = st.ListPersonBriefVersionsContext(t.Context(), person, 10)
			require.NoError(err)
			require.Len(history, 1)
			require.NotNil(history[0].SupersededAt)
			assert.Equal(retiredAt, *history[0].SupersededAt)
			assert.Equal(beforeBrief, f.runner.briefRequests)
			assert.Equal(beforeExtraction, f.runner.extractionRequests)
		})
	}
}

// A capacity-retired event must not leave an obsolete current paragraph when
// the paid replacement has no renderable content. History keeps the watermark.
func TestPersonSweepEventCapacityEmptyRetiresCurrent(t *testing.T) {
	for _, tc := range []struct {
		name      string
		preflight bool
	}{
		{"projection", false},
		{"preflight", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checks := assert.New(t)
			required := require.New(t)
			f, now, _ := newStoreEventFixture(t, "capacity-empty-"+tc.name)
			st, person := f.journal.store, f.journal.alicePersonID
			runStoreEvent(t, f, peoplesweep.BriefModeForce)
			prior, err := st.GetPersonBriefContext(t.Context(), person, 0)
			required.NoError(err)
			var boundary peoplesweep.BriefBoundary
			required.NoError(json.Unmarshal(prior.Boundary, &boundary))
			required.Len(boundary.EventMemory, 1)
			f.worker.Config.Brief.MaxItems = 1
			f.worker.Config.Brief.OverlapItems = 0
			*now = time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
			if tc.preflight {
				owner, err := st.EnsureParticipant("owner@example.test", "Owner", "example.test")
				required.NoError(err)
				required.NoError(st.AddAccountIdentity(f.journal.sourceID, "owner@example.test", "manual"))
				for _, participant := range []int64{owner, f.journal.aliceID} {
					required.NoError(st.EnsureConversationParticipant(f.journal.conversationID, participant, "member"))
				}
				message := f.journal.insertMessage(t, "capacity-new-contact", "chat", f.journal.aliceID, now.Add(-time.Hour))
				addSweepBody(t, f.journal, message, "An ordinary update.")
				projector, err := activity.NewProjector(st, activity.Options{})
				required.NoError(err)
				_, err = projector.RunOnce(t.Context())
				required.NoError(err)
				last, present, err := st.PersonSweepLastContact(t.Context(), person)
				required.NoError(err)
				required.True(present)
				required.Equal(message, last.MessageID)
			}
			f.runner.candidate = func(map[string]any) json.RawMessage { return emptyEventBrief() }
			briefBefore, extractionBefore := f.runner.briefRequests, f.runner.extractionRequests
			result := runStoreEvent(t, f, peoplesweep.BriefModeAuto)
			required.Equal(1, result.PeopleSucceeded)
			checks.Equal(briefBefore+1, f.runner.briefRequests)
			_, err = st.GetPersonBriefContext(t.Context(), person, 0)
			required.ErrorIs(err, store.ErrPersonBriefNotFound)
			history, err := st.ListPersonBriefVersionsContext(t.Context(), person, 10)
			required.NoError(err)
			required.Len(history, 1)
			checks.Equal(store.PersonBriefStatusSuperseded, history[0].Status)
			checks.NotNil(history[0].SupersededAt)
			checks.Equal(prior.Boundary, history[0].Boundary)
			required.Len(result.People, 1)
			checks.Equal(peoplesweep.FailureInternal, result.People[0].BriefFailureClass)
			checks.Equal((f.runner.briefRequests-briefBefore)+(f.runner.extractionRequests-extractionBefore),
				result.People[0].Usage.Requests, "paid inference remains accounted for")
		})
	}
}
