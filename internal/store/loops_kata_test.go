package store_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func insertKataTestLink(t *testing.T, f *storetest.Fixture, id int64, now time.Time) store.LoopKataLink {
	t.Helper()
	link, inserted, err := f.Store.InsertLoopKataLinkContext(t.Context(), store.LoopKataLink{LoopID: id, IssueUID: "synthetic-issue", IssueRef: "example#ab12", IssueTitle: "Reply to peer", IssueStatus: "open", LinkedVia: "created", LinkedAt: now}, "")
	require.NoError(t, err)
	require.True(t, inserted)
	return link
}

func TestLoopKataConditionalInsertAndToken(t *testing.T) {
	f := storetest.New(t)
	s := f.Store
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	id := insertKataSchemaLoop(t, f, "linked", "reply_owed", "turn_state")
	link := insertKataTestLink(t, f, id, now)
	assert.Len(t, link.LinkToken, 32)
	winner, inserted, err := s.InsertLoopKataLinkContext(ctx, store.LoopKataLink{LoopID: id, IssueUID: "other", IssueRef: "example#zz99", LinkedVia: "existing", LinkedAt: now}, "")
	require.NoError(t, err)
	assert.False(t, inserted)
	assert.Equal(t, link.LinkToken, winner.LinkToken)
	assert.Equal(t, "synthetic-issue", winner.IssueUID)
	rows, err := s.ListOpenLoopsContext(ctx, store.LoopListFilter{Limit: 10, Now: now})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].Kata)
	assert.Equal(t, link.IssueRef, rows[0].Kata.IssueRef)
	require.NoError(t, s.DeleteLoopKataLinkContext(ctx, id))
	replacement := insertKataTestLink(t, f, id, now)
	assert.NotEqual(t, link.LinkToken, replacement.LinkToken)
	link.IssueTitle = "Stale"
	require.NoError(t, s.UpdateLoopKataLinkContext(ctx, link))
	require.NoError(t, s.ReconcileLoopKataContext(ctx, rows[0], true, now, 7*24*time.Hour))
	current, err := s.GetOpenLoopContext(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "open", current.Status)
	assert.Equal(t, "Reply to peer", current.Kata.IssueTitle)
	kataLoopExec(t, s, "DELETE FROM open_loops WHERE id=?", id)
	_, err = s.GetLoopKataLinkContext(ctx, id)
	assert.ErrorIs(t, err, store.ErrLoopKataLinkNotFound)
}

func TestLoopKataCloseReopenAndBlocked(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(map[bool]string{false: "open", true: "held"}[held], func(t *testing.T) {
			f := storetest.New(t)
			s := f.Store
			ctx := t.Context()
			now := time.Now().UTC().Truncate(time.Second)
			id := insertKataSchemaLoop(t, f, "bridge", "reply_owed", "turn_state")
			insertKataTestLink(t, f, id, now)
			if held {
				kataLoopExec(t, s, "UPDATE open_loops SET opens_at=? WHERE id=?", kataLoopStamp(s, now.Add(time.Hour)), id)
			}
			observed, err := s.GetOpenLoopContext(ctx, id)
			require.NoError(t, err)
			require.NoError(t, s.ReconcileLoopKataContext(ctx, observed, true, now, 7*24*time.Hour))
			closed, err := s.GetOpenLoopContext(ctx, id)
			require.NoError(t, err)
			assert.Equal(t, "closed", closed.Status)
			assert.Equal(t, "done", *closed.ClosedReason)
			assert.Equal(t, "system", *closed.ClosedBy)
			assert.Contains(t, string(closed.EvidenceJSON), "kata_closed_ref")
			require.NotNil(t, closed.Kata.BridgeClosedLoopAt)
			require.NoError(t, s.ReconcileLoopKataContext(ctx, closed, false, now.Add(time.Minute), 7*24*time.Hour))
			reopened, err := s.GetOpenLoopContext(ctx, id)
			require.NoError(t, err)
			assert.Equal(t, map[bool]string{false: "open", true: "held"}[held], reopened.Status)
			assert.Nil(t, reopened.ClosedAt)
			assert.Nil(t, reopened.Kata.BridgeClosedLoopAt)
			queued, err := s.PendingLoopConversationsContext(ctx, 10)
			require.NoError(t, err)
			require.Len(t, queued, 1)
			assert.Equal(t, "reopen", queued[0].Reason)
			require.NoError(t, s.ReconcileLoopKataContext(ctx, reopened, true, now.Add(2*time.Minute), 7*24*time.Hour))
			closed, err = s.GetOpenLoopContext(ctx, id)
			require.NoError(t, err)
			newer := insertKataSchemaLoop(t, f, "newer", "reply_owed", "turn_state")
			require.NoError(t, s.ReconcileLoopKataContext(ctx, closed, false, now.Add(3*time.Minute), 7*24*time.Hour))
			blocked, err := s.GetOpenLoopContext(ctx, id)
			require.NoError(t, err)
			assert.Equal(t, "closed", blocked.Status)
			assert.Contains(t, string(blocked.EvidenceJSON), "kata_reopen_blocked")
			assert.Nil(t, blocked.Kata.BridgeClosedLoopAt)
			kataLoopExec(t, s, "DELETE FROM open_loops WHERE id=?", newer)
			require.NoError(t, s.ReconcileLoopKataContext(ctx, blocked, false, now.Add(4*time.Minute), 7*24*time.Hour))
			blocked, err = s.GetOpenLoopContext(ctx, id)
			require.NoError(t, err)
			assert.Equal(t, "closed", blocked.Status)
		})
	}
}

func TestLoopKataReopenRespectsCurrentSuppressions(t *testing.T) {
	for _, targetKind := range []string{"conversation", "participant", "person"} {
		t.Run(targetKind, func(t *testing.T) {
			f := storetest.New(t)
			s := f.Store
			ctx := t.Context()
			now := time.Now().UTC().Truncate(time.Second)
			id := insertKataSchemaLoop(t, f, "suppressed-reopen", "reply_owed", "turn_state")

			participantID := f.EnsureParticipant("peer@example.test", "Example Peer", "example.test")
			require.NoError(t, s.EnsureConversationParticipant(f.ConvID, participantID, "member"))
			var personID int64
			require.NoError(t, s.DB().QueryRowContext(ctx, s.Rebind("INSERT INTO persons(vcard_uid) VALUES(?) RETURNING id"), "reopen-suppression-person").Scan(&personID))
			kataLoopExec(t, s, "INSERT INTO person_participants(person_id,participant_id) VALUES(?,?)", personID, participantID)
			kataLoopExec(t, s, "UPDATE open_loops SET counterparty_participant_id=?,counterparty_person_id=? WHERE id=?", participantID, personID, id)
			insertKataTestLink(t, f, id, now)

			observed, err := s.GetOpenLoopContext(ctx, id)
			require.NoError(t, err)
			require.NoError(t, s.ReconcileLoopKataContext(ctx, observed, true, now, 7*24*time.Hour))
			closed, err := s.GetOpenLoopContext(ctx, id)
			require.NoError(t, err)
			require.Equal(t, "closed", closed.Status)
			require.NotNil(t, closed.Kata.BridgeClosedLoopAt)

			request := store.LoopSuppressionRequest{TargetKind: targetKind}
			switch targetKind {
			case "conversation":
				request.ConversationID = &f.ConvID
			case "participant":
				request.ParticipantID = &participantID
			case "person":
				request.PersonID = &personID
			}
			_, err = s.CreateLoopSuppressionContext(ctx, request, now.Add(time.Minute))
			require.NoError(t, err)

			require.NoError(t, s.ReconcileLoopKataContext(ctx, closed, false, now.Add(2*time.Minute), 7*24*time.Hour))
			current, err := s.GetOpenLoopContext(ctx, id)
			require.NoError(t, err)
			assert.Equal(t, "closed", current.Status)
			require.NotNil(t, current.ClosedReason)
			assert.Equal(t, "done", *current.ClosedReason)
			assert.Nil(t, current.Kata.BridgeClosedLoopAt)

			pending, err := s.PendingLoopConversationsContext(ctx, 10)
			require.NoError(t, err)
			for _, queued := range pending {
				assert.NotEqual(t, "reopen", queued.Reason)
			}
		})
	}
}

func TestLoopKataOwnerCloseWins(t *testing.T) {
	f := storetest.New(t)
	s := f.Store
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	id := insertKataSchemaLoop(t, f, "owner", "reply_owed", "turn_state")
	insertKataTestLink(t, f, id, now)
	observed, err := s.GetOpenLoopContext(ctx, id)
	require.NoError(t, err)
	_, err = s.CloseLoopContext(ctx, id, "", now)
	require.NoError(t, err)
	require.NoError(t, s.ReconcileLoopKataContext(ctx, observed, true, now, 7*24*time.Hour))
	require.NoError(t, s.ReconcileLoopKataContext(ctx, observed, false, now, 7*24*time.Hour))
	current, err := s.GetOpenLoopContext(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "user", *current.ClosedBy)
	assert.Nil(t, current.Kata.BridgeClosedLoopAt)
}

func TestLoopKataWorkFairnessAndFlags(t *testing.T) {
	f := storetest.New(t)
	s := f.Store
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	ids := []int64{}
	for i, key := range []string{"first", "second", "third", "fourth"} {
		id := insertKataSchemaLoop(t, f, key, "commitment_owed_by_me", "commitment_extraction")
		ids = append(ids, id)
		link := insertKataTestLink(t, f, id, now)
		at := now.Add(time.Duration(i-2) * time.Minute)
		link.NextAttemptAt = &at
		require.NoError(t, s.UpdateLoopKataLinkContext(ctx, link))
	}
	rows, err := s.LoopKataWorkContext(ctx, now, 7*24*time.Hour, 1, true, true)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, ids[0], rows[0].ID)
	kataLoopExec(t, s, "UPDATE open_loops SET status='closed',closed_at=?,closed_reason='turn_flip',closed_by='system' WHERE id=?", kataLoopStamp(s, now), ids[0])
	rows, err = s.LoopKataWorkContext(ctx, now, 7*24*time.Hour, 10, false, false)
	require.NoError(t, err)
	assert.Len(t, rows, 2)
	rows, err = s.LoopKataWorkContext(ctx, now, 7*24*time.Hour, 10, true, false)
	require.NoError(t, err)
	assert.Len(t, rows, 3)
	link, err := s.GetLoopKataLinkContext(ctx, ids[0])
	require.NoError(t, err)
	link.IssueStatus = "missing"
	require.NoError(t, s.UpdateLoopKataLinkContext(ctx, link))
	rows, err = s.LoopKataWorkContext(ctx, now, 7*24*time.Hour, 10, true, false)
	require.NoError(t, err)
	assert.Len(t, rows, 2)
}

func TestLoopKataAgendaAndState(t *testing.T) {
	f := storetest.New(t)
	s := f.Store
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	var person int64
	require.NoError(t, s.DB().QueryRow("INSERT INTO persons(vcard_uid) VALUES('loop-counterparty') RETURNING id").Scan(&person))
	id := insertKataSchemaLoop(t, f, "agenda", "reply_owed", "turn_state")
	kataLoopExec(t, s, "UPDATE open_loops SET counterparty_person_id=?,agenda_state='suggested' WHERE id=?", person, id)
	input := store.LoopKataLink{LoopID: id, IssueUID: "agenda-issue", IssueRef: "example#ab12", LinkedVia: "existing", LinkedAt: now}
	_, _, err := s.InsertLoopKataLinkContext(ctx, input, "someone-else")
	require.NoError(t, err)
	row, err := s.GetOpenLoopContext(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "suggested", row.AgendaState)
	require.NoError(t, s.DeleteLoopKataLinkContext(ctx, id))
	_, _, err = s.InsertLoopKataLinkContext(ctx, input, "loop-counterparty")
	require.NoError(t, err)
	row, err = s.GetOpenLoopContext(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "created", row.AgendaState)
	require.NoError(t, s.RecordLoopKataRunContext(ctx, now, nil))
	require.NoError(t, s.RecordLoopKataRunContext(ctx, now.Add(time.Minute), errors.New("offline")))
	state, err := s.LoopDerivationStateContext(ctx)
	require.NoError(t, err)
	require.NotNil(t, state.KataLastSuccessAt)
	assert.True(t, now.Equal(*state.KataLastSuccessAt))
	assert.Equal(t, "offline", state.KataLastError)
	require.NotNil(t, state.KataLastRunAt)
	assert.True(t, now.Add(time.Minute).Equal(*state.KataLastRunAt))
}

func TestLoopKataReopenSelectionAndWindow(t *testing.T) {
	f := storetest.New(t)
	s := f.Store
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	id := insertKataSchemaLoop(t, f, "reopen-window", "reply_owed", "turn_state")
	insertKataTestLink(t, f, id, now)
	observed, err := s.GetOpenLoopContext(ctx, id)
	require.NoError(t, err)
	require.NoError(t, s.ReconcileLoopKataContext(ctx, observed, true, now, 7*24*time.Hour))
	rows, err := s.LoopKataWorkContext(ctx, now, time.Hour, 10, false, false)
	require.NoError(t, err)
	assert.Empty(t, rows)
	rows, err = s.LoopKataWorkContext(ctx, now, time.Hour, 10, false, true)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NoError(t, s.ReconcileLoopKataContext(ctx, rows[0], false, now.Add(2*time.Hour), time.Hour))
	current, err := s.GetOpenLoopContext(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "closed", current.Status)
	rows, err = s.LoopKataWorkContext(ctx, now.Add(2*time.Hour), time.Hour, 10, true, true)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestLoopKataUntrackedAndOwnerEdit(t *testing.T) {
	f := storetest.New(t)
	s := f.Store
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	id := insertKataSchemaLoop(t, f, "owner-edit", "reply_owed", "turn_state")
	row, err := s.GetOpenLoopContext(ctx, id)
	require.NoError(t, err)
	assert.Nil(t, row.Kata)
	rows, err := s.ListLiveConversationLoopsContext(ctx, f.ConvID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Nil(t, rows[0].Kata)
	insertKataTestLink(t, f, id, now)
	observed, err := s.GetOpenLoopContext(ctx, id)
	require.NoError(t, err)
	_, err = s.SnoozeLoopContext(ctx, id, now.Add(2*time.Hour), now.Add(time.Minute))
	require.NoError(t, err)
	require.NoError(t, s.ReconcileLoopKataContext(ctx, observed, true, now.Add(2*time.Minute), 7*24*time.Hour))
	current, err := s.GetOpenLoopContext(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "open", current.Status)
}

func TestLoopKataSystemDoneRemainsTerminal(t *testing.T) {
	f := storetest.New(t)
	s := f.Store
	now := time.Now().UTC().Truncate(time.Second)
	ids := f.CreateMessages(2)
	open := kataLoopOpen(t, f, ids[0], ids[1], now)
	kataApplyLoop(t, f, open, now)
	rows, err := s.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	insertKataTestLink(t, f, rows[0].ID, now)
	row, err := s.GetOpenLoopContext(t.Context(), rows[0].ID)
	require.NoError(t, err)
	require.NoError(t, s.ReconcileLoopKataContext(t.Context(), row, true, now, 7*24*time.Hour))
	kataApplyLoop(t, f, open, now.Add(time.Minute))
	rows, err = s.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestLoopKataCacheRoundTripPreservesLifecycle(t *testing.T) {
	f := storetest.New(t)
	s := f.Store
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	id := insertKataSchemaLoop(t, f, "cache", "reply_owed", "turn_state")
	link := insertKataTestLink(t, f, id, now)
	observed, err := s.GetOpenLoopContext(ctx, id)
	require.NoError(t, err)
	require.NoError(t, s.ReconcileLoopKataContext(ctx, observed, true, now, time.Hour))
	at := now.Add(time.Minute)
	link.IssueTitle = "Updated title"
	link.IssueStatus = "closed"
	link.IssueWebURL = "https://example.test/issue"
	link.CheckedAt = &at
	link.Attempts = 3
	link.NextAttemptAt = &at
	link.LastError = "offline"
	link.ClosedNotifiedAt = &at
	link.UpdatedAt = at
	require.NoError(t, s.UpdateLoopKataLinkContext(ctx, link))
	got, err := s.GetLoopKataLinkContext(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "Updated title", got.IssueTitle)
	assert.Equal(t, 3, got.Attempts)
	assert.Equal(t, "offline", got.LastError)
	require.NotNil(t, got.BridgeClosedLoopAt)
	require.NotNil(t, got.ClosedNotifiedAt)
	assert.True(t, at.Equal(*got.ClosedNotifiedAt))
	assert.True(t, now.Equal(*got.BridgeClosedLoopAt))
	link.ClosedNotifiedAt = nil
	link.Attempts = 0
	link.LastError = ""
	require.NoError(t, s.UpdateLoopKataLinkContext(ctx, link))
	got, err = s.GetLoopKataLinkContext(ctx, id)
	require.NoError(t, err)
	assert.Zero(t, got.Attempts)
	assert.Empty(t, got.LastError)
	assert.NotNil(t, got.ClosedNotifiedAt)
	require.NoError(t, s.DeleteLoopKataLinkContext(ctx, id))
	err = s.DeleteLoopKataLinkContext(ctx, id)
	assert.ErrorIs(t, err, store.ErrLoopNotFound)
}

func kataLoopExec(t *testing.T, s *store.Store, query string, args ...any) {
	t.Helper()
	_, err := s.DB().ExecContext(t.Context(), s.Rebind(query), args...)
	require.NoError(t, err)
}
func kataLoopStamp(s *store.Store, at time.Time) any {
	if s.IsPostgreSQL() {
		return at
	}
	return at.UTC().Format(store.SQLiteTimestampLayout)
}
func insertKataSchemaLoop(t *testing.T, f *storetest.Fixture, key, kind, detector string) int64 {
	t.Helper()
	var id int64
	err := f.Store.DB().QueryRow(f.Store.Rebind(`INSERT INTO open_loops (turn_key,conversation_id,source_id,kind,detector,status,channel,owed_since_at,last_activity_at,opens_at,evidence_json,ruleset_version) VALUES (?,?,?,?,?,'open','email',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,'[]',1) RETURNING id`), key, f.ConvID, f.Source.ID, kind, detector).Scan(&id)
	require.NoError(t, err)
	return id
}
func kataLoopOpen(t *testing.T, f *storetest.Fixture, anchor, last int64, now time.Time) store.LoopOpenDecision {
	t.Helper()
	return store.LoopOpenDecision{TurnKey: fmt.Sprintf("turn:%d:reply_owed:%d", f.ConvID, anchor), Kind: "reply_owed", Status: "open", Channel: "email", ConversationID: f.ConvID, SourceID: f.Source.ID, AnchorMessageID: anchor, LastActivityMessageID: last, OwedSinceAt: now.Add(-48 * time.Hour), LastActivityAt: now, OpensAt: now.Add(-24 * time.Hour), EvidenceJSON: []byte("[]"), RulesetVersion: 1, TurnMessageIDs: []int64{anchor, last}}
}
func kataApplyLoop(t *testing.T, f *storetest.Fixture, open store.LoopOpenDecision, now time.Time) {
	t.Helper()
	require.NoError(t, f.Store.ApplyLoopVerdictContext(t.Context(), store.LoopApply{ConversationID: f.ConvID, Now: now, Verdict: store.LoopVerdict{Open: &open}}))
}

func TestLoopKataClosedTrackAndCounterpartyAlias(t *testing.T) {
	f := storetest.New(t)
	s := f.Store
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	var person int64
	require.NoError(t, s.DB().QueryRow("INSERT INTO persons(vcard_uid) VALUES('current-loop-person') RETURNING id").Scan(&person))
	kataLoopExec(t, s, "INSERT INTO person_uid_aliases(retired_uid,surviving_person_id,reason) VALUES('retired-loop-person',?,'merge')", person)
	id := insertKataSchemaLoop(t, f, "closed-in-flight", "reply_owed", "turn_state")
	kataLoopExec(t, s, "UPDATE open_loops SET counterparty_person_id=? WHERE id=?", person, id)
	_, err := s.CloseLoopContext(ctx, id, "", now)
	require.NoError(t, err)
	_, inserted, err := s.InsertLoopKataLinkContext(ctx, store.LoopKataLink{LoopID: id, IssueUID: "alias-issue", IssueRef: "example#ab12", LinkedVia: "created", LinkedAt: now}, "retired-loop-person")
	require.NoError(t, err)
	assert.True(t, inserted)
	row, err := s.GetOpenLoopContext(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "closed", row.Status)
	assert.Equal(t, "created", row.AgendaState)
}

func TestLoopKataCommitmentReopenBlockedByTurnKey(t *testing.T) {
	f := storetest.New(t)
	s := f.Store
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	id := insertKataSchemaLoop(t, f, "commit-cycle", "commitment_owed_by_me", "commitment_extraction")
	insertKataTestLink(t, f, id, now)
	row, err := s.GetOpenLoopContext(ctx, id)
	require.NoError(t, err)
	require.NoError(t, s.ReconcileLoopKataContext(ctx, row, true, now, time.Hour))
	row, err = s.GetOpenLoopContext(ctx, id)
	require.NoError(t, err)
	kataLoopExec(t, s, `INSERT INTO open_loops(turn_key,cycle,conversation_id,source_id,kind,detector,status,channel,owed_since_at,last_activity_at,opens_at,evidence_json,ruleset_version) SELECT turn_key,2,conversation_id,source_id,kind,detector,'open',channel,owed_since_at,last_activity_at,opens_at,evidence_json,ruleset_version FROM open_loops WHERE id=?`, id)
	require.NoError(t, s.ReconcileLoopKataContext(ctx, row, false, now.Add(time.Minute), time.Hour))
	row, err = s.GetOpenLoopContext(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "closed", row.Status)
	assert.Nil(t, row.Kata.BridgeClosedLoopAt)
	assert.Contains(t, string(row.EvidenceJSON), "kata_reopen_blocked")
}
