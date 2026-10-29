package store

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func TestPeopleWorkersColdPolicyPostgresSourceEditCannotCrossPublication(t *testing.T) {
	requirements := require.New(t)

	f := newPersonSweepApplyFixture(t, "policy-source-lock", true)
	st := f.store
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL row-lock interleaving")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	source, err := st.GetOrCreateSource("slack", "policy-source-lock")
	requirements.NoError(err)
	conversation, err := st.EnsureConversationWithType(source.ID, "policy-source-lock", "direct_chat", "Synthetic lock chat")
	requirements.NoError(err)
	requirements.NoError(st.SetConversationPolicyOverride(ctx, source.ID, "policy-source-lock", conversationpolicy.Full))
	var participant int64
	requirements.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT participant_id FROM person_participants WHERE person_id=?`), f.personID).Scan(&participant))
	message, err := st.UpsertMessage(&Message{SourceID: source.ID, SourceMessageID: "synthetic-input", ConversationID: conversation, MessageType: "chat", SenderID: sql.NullInt64{Int64: participant, Valid: true}})
	requirements.NoError(err)
	requirements.NoError(st.UpsertMessageBodyContext(ctx, message, sql.NullString{String: "Original source input", Valid: true}, sql.NullString{}))
	inputs, _, err := st.hydratePersonSweepMessageSet(ctx, f.personID, peoplesweep.SourceConversationText, []int64{message}, 0)
	requirements.NoError(err)
	requirements.Len(inputs, 1)
	work, err := st.CapturePersonSweepPolicyWork(ctx, f.personID)
	requirements.NoError(err)
	work, err = st.BindPersonSweepPolicyInputs(work, inputs)
	requirements.NoError(err)
	peer, err := OpenForTest(st.dbPath)
	requirements.NoError(err)
	defer func() { require.NoError(t, peer.Close()) }()
	st.DB().SetMaxOpenConns(1)
	peer.DB().SetMaxOpenConns(1)
	_, err = peer.DB().ExecContext(ctx, `SET lock_timeout='250ms'`)
	requirements.NoError(err)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	work = withPersonSweepApplyFailpoint(work, func(stage string) error {
		if stage == "locks" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	done := make(chan error, 1)
	go func() { _, err := st.ApplyPersonSweep(work, f.request); done <- err }()
	select {
	case <-entered:
	case err := <-done:
		requirements.NoError(err)
		requirements.FailNow("publication did not reach locked stage")
	case <-ctx.Done():
		requirements.NoError(ctx.Err())
	}
	err = peer.UpsertMessageBodyContext(ctx, message, sql.NullString{String: "Edited source input", Valid: true}, sql.NullString{})
	requirements.ErrorContains(err, "lock timeout", "the actual body writer must wait for publication's source lock")
	once.Do(func() { close(release) })
	requirements.NoError(<-done)
	requirements.NoError(peer.UpsertMessageBodyContext(ctx, message, sql.NullString{String: "Edited source input", Valid: true}, sql.NullString{}))
	requirements.ErrorIs(CheckConversationPolicyAdmission(work), ErrConversationPolicyStale)
}

func TestPeopleWorkersColdPolicyPostgresPersonEditBeforeDerivedWrite(t *testing.T) {
	requirements := require.New(t)

	f := newPersonSweepApplyFixture(t, "policy-person-lock", true)
	st := f.store
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL publication interleaving")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	work, err := st.CapturePersonSweepPolicyWork(ctx, f.personID)
	requirements.NoError(err)
	peer, err := OpenForTest(st.dbPath)
	requirements.NoError(err)
	defer func() { require.NoError(t, peer.Close()) }()
	st.DB().SetMaxOpenConns(1)
	peer.DB().SetMaxOpenConns(1)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	work = withPersonSweepApplyFailpoint(work, func(stage string) error {
		if stage == "locks" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	done := make(chan error, 1)
	go func() { _, err := st.ApplyPersonSweep(work, f.request); done <- err }()
	select {
	case <-entered:
	case err := <-done:
		requirements.NoError(err)
		requirements.FailNow("publication did not reach locked stage")
	case <-ctx.Done():
		requirements.NoError(ctx.Err())
	}
	// Person locking follows the existing fact target/catalog/organization locks.
	// A change before that lock must be rejected at the final validation point.
	_, err = peer.DB().ExecContext(ctx, peer.Rebind(`UPDATE persons SET display_name=? WHERE id=?`), "Changed synthetic person", f.personID)
	requirements.NoError(err)
	once.Do(func() { close(release) })
	requirements.ErrorIs(<-done, ErrConversationPolicyStale)
	var generations int
	requirements.NoError(st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM person_fact_generations`).Scan(&generations))
	requirements.Zero(generations)
}
