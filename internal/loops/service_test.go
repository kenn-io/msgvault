package loops

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/freshness"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func serviceFixture(t *testing.T) (*Service, *storetest.Fixture, time.Time, int64) {
	t.Helper()
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	participant := f.EnsureParticipant("person@example.test", "Example Person", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.Accounts = []config.AccountSchedule{{Email: f.Source.Identifier, Enabled: false}}
	service, err := NewService(f.Store, cfg, func(context.Context) (freshness.ProjectionState, error) {
		return freshness.ProjectionState{ComputedAt: &now}, nil
	})
	require.NoError(t, err)
	service.now = func() time.Time { return now }
	for i := range 3 {
		conversation, err := f.Store.EnsureConversation(f.Source.ID, fmt.Sprintf("turn-%d", i), "A question")
		require.NoError(t, err)
		message := f.CreateMessage(fmt.Sprintf("anchor-%d", i))
		_, err = f.Store.DB().Exec(f.Store.Rebind("UPDATE messages SET conversation_id=? WHERE id=?"), conversation, message)
		require.NoError(t, err)
		at := now.Add(-time.Duration(72-i) * time.Hour)
		evidence, err := json.Marshal([]Evidence{{MessageID: message, ConversationID: conversation, OccurredAt: at, Subject: "A question"}})
		require.NoError(t, err)
		_, err = f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO open_loops(id,turn_key,conversation_id,source_id,kind,detector,status,channel,counterparty_participant_id,counterparty_person_id,anchor_message_id,owed_since_at,last_activity_message_id,last_activity_at,opens_at,summary,evidence_json,ruleset_version) VALUES(?,?,?,?,?,'turn_state','open','email',?,?,?,?,?,?,?,?,?,1)`), i+1, fmt.Sprintf("turn:%d:reply_owed:%d", conversation, message), conversation, f.Source.ID, ReplyOwed, participant, person.ID, message, at, message, at, at.Add(24*time.Hour), "A question", string(evidence))
		require.NoError(t, err)
	}
	return service, f, now, person.ID
}

func TestServiceListPaginationAndReader(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	s, _, now, personID := serviceFixture(t)
	page, err := s.List(t.Context(), ListOptions{Limit: 2})
	requirements.NoError(err)
	requirements.Len(page.Items, 2)
	assertions.Equal(int64(1), page.Items[0].ID)
	assertions.NotEmpty(page.NextCursor)
	next, err := s.List(t.Context(), ListOptions{Limit: 2, Cursor: page.NextCursor})
	requirements.NoError(err)
	requirements.Len(next.Items, 1)
	assertions.Equal(int64(3), next.Items[0].ID)
	assertions.Empty(next.NextCursor)
	reader, err := s.ListPersonOpenLoops(t.Context(), personID, 2)
	requirements.NoError(err)
	assertions.Len(reader.Loops, 2)
	assertions.True(reader.Truncated)
	assertions.Equal(now, reader.ComputedAt)
	_, err = s.List(t.Context(), ListOptions{Cursor: "invalid"})
	requirements.Error(err)
}

func TestServiceWaitingFreshnessAndGrouping(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	s, _, now, _ := serviceFixture(t)
	waiting, err := s.Waiting(t.Context(), WaitingOptions{Top: 1})
	requirements.NoError(err)
	requirements.Len(waiting.Groups, 1)
	assertions.Equal(3, waiting.Count)
	assertions.Equal(3, waiting.Groups[0].LoopCount)
	assertions.Equal(now.Add(-72*time.Hour), waiting.Groups[0].OldestOwedSinceAt)
	s.projection = func(context.Context) (freshness.ProjectionState, error) {
		return freshness.ProjectionState{}, nil
	}
	_, err = s.Waiting(t.Context(), WaitingOptions{})
	var stale *StaleError
	requirements.ErrorAs(err, &stale)
	assertions.Equal("loops_stale", stale.Response.Error)
	assertions.True(stale.Response.Freshness.Stale)
	waiting, err = s.Waiting(t.Context(), WaitingOptions{AllowStale: true})
	requirements.NoError(err)
	assertions.True(waiting.Freshness.Stale)
	assertions.Equal(3, waiting.Count)
	detail, err := s.Get(t.Context(), 1)
	requirements.NoError(err)
	assertions.True(detail.Freshness.Stale)
	requirements.Len(detail.Loop.Evidence, 1)
	assertions.Equal("A question", detail.Loop.Evidence[0].Subject)
}

func TestServiceWaitingExcludesSnoozedAndAwaitingReply(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	s, f, now, _ := serviceFixture(t)
	_, err := f.Store.DB().Exec(f.Store.Rebind("UPDATE open_loops SET snoozed_until=? WHERE id=1"), now.Add(time.Hour))
	requirements.NoError(err)
	_, err = f.Store.DB().Exec("UPDATE open_loops SET kind='awaiting_reply' WHERE id=2")
	requirements.NoError(err)
	waiting, err := s.Waiting(t.Context(), WaitingOptions{})
	requirements.NoError(err)
	assertions.Equal(1, waiting.Count)
	requirements.Len(waiting.Groups, 1)
	requirements.Len(waiting.Groups[0].Loops, 1)
	assertions.Equal(int64(3), waiting.Groups[0].Loops[0].ID)
}

func TestServiceWaitingAsOfKeepsCurrentSnoozeEligibility(t *testing.T) {
	for _, tc := range []struct {
		name            string
		asOf, snoozeEnd time.Duration
		wantIDs         []int64
		wantAge         int64
		wantStale       bool
	}{
		{name: "future evaluation keeps active snooze excluded", asOf: 48 * time.Hour, snoozeEnd: time.Hour, wantIDs: []int64{2, 3}, wantAge: 119 * 3600, wantStale: true},
		{name: "past evaluation keeps expired snooze included", asOf: -2 * time.Hour, snoozeEnd: -time.Hour, wantIDs: []int64{1, 2, 3}, wantAge: 70 * 3600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			s, f, now, _ := serviceFixture(t)
			_, err := f.Store.DB().Exec(f.Store.Rebind("UPDATE open_loops SET snoozed_until=? WHERE id=1"), now.Add(tc.snoozeEnd))
			requirements.NoError(err)
			waiting, err := s.Waiting(t.Context(), WaitingOptions{AsOf: now.Add(tc.asOf), AllowStale: true})
			requirements.NoError(err)
			assertions.Equal(len(tc.wantIDs), waiting.Count)
			requirements.Len(waiting.Groups, 1)
			var ids []int64
			for _, loop := range waiting.Groups[0].Loops {
				ids = append(ids, loop.ID)
			}
			assertions.Equal(tc.wantIDs, ids)
			requirements.NotEmpty(waiting.Groups[0].Loops)
			assertions.Equal(tc.wantAge, waiting.Groups[0].Loops[0].AgeSeconds)
			assertions.Equal(now.Add(tc.asOf), waiting.Freshness.AsOf)
			assertions.Equal(tc.wantStale, waiting.Freshness.Stale)
			if tc.wantStale {
				assertions.Contains(waiting.Freshness.StaleReasons, "projection_stale:open_loops")
			}
		})
	}
}
