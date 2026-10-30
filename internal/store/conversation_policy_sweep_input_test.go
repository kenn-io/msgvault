package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
)

func TestPeopleWorkersColdPolicyChecksOriginalSweepInput(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := newPersonSweepJournalFixture(t, true, false)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	id := f.insertMessage(t, "policy-original-input", "chat", f.aliceID, time.Now().UTC())
	addSweepBody(t, f, id, "Original synthetic input")
	window := loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0)
	requirements.NotEmpty(window.Seeds)
	f.store.DB().SetMaxOpenConns(1)
	work, err := f.store.CapturePersonSweepPolicyWork(ctx, f.alicePersonID)
	requirements.NoError(err)
	work, err = f.store.BindPersonSweepPolicyInputs(work, window.Seeds)
	requirements.NoError(err)
	requirements.NoError(store.CheckConversationPolicyAdmission(work))
	before, err := f.store.ConversationPolicyRevision(ctx)
	requirements.NoError(err)
	_, err = f.store.DB().ExecContext(ctx, f.store.Rebind(`UPDATE message_bodies SET body_text=? WHERE message_id=?`), "Changed synthetic input", id)
	requirements.NoError(err)
	after, err := f.store.ConversationPolicyRevision(ctx)
	requirements.NoError(err)
	assertions.Equal(before, after)
	assertions.ErrorIs(store.CheckConversationPolicyAdmission(work), store.ErrConversationPolicyStale)
}

func TestPeopleWorkersColdPolicyChecksCanonicalPersonInput(t *testing.T) {
	requirements := require.New(t)

	f := newPersonSweepJournalFixture(t, true, false)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	f.store.DB().SetMaxOpenConns(1)
	work, err := f.store.CapturePersonSweepPolicyWork(ctx, f.alicePersonID)
	requirements.NoError(err)
	before, err := f.store.LoadPersonSemanticDocumentContext(ctx, f.alicePersonID)
	requirements.NoError(err)
	// A persisted semantic field can change independently of the policy epoch.
	// Keep persons.revision unchanged to distinguish the canonical text fingerprint.
	_, err = f.store.DB().ExecContext(ctx, f.store.Rebind(`UPDATE persons SET display_name=? WHERE id=?`), "Changed synthetic name", f.alicePersonID)
	requirements.NoError(err)
	after, err := f.store.LoadPersonSemanticDocumentContext(ctx, f.alicePersonID)
	requirements.NoError(err)
	requirements.NotEqual(before.Revision, after.Revision)
	assert.ErrorIs(t, store.CheckConversationPolicyAdmission(work), store.ErrConversationPolicyStale)
}

func TestPeopleWorkersColdPolicyChecksDocumentInput(t *testing.T) {
	requirements := require.New(t)

	f, personID := newPersonSweepDocumentFixture(t)
	st := f.Store
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	inputs, err := st.SearchPersonSweepDocuments(ctx, peoplesweep.DocumentContextRequest{PersonID: personID, Query: "synthetic", Limit: 10})
	requirements.NoError(err)
	requirements.Len(inputs, 1)
	st.DB().SetMaxOpenConns(1)
	work, err := st.CapturePersonSweepPolicyWork(ctx, personID)
	requirements.NoError(err)
	work, err = st.BindPersonSweepPolicyInputs(work, inputs)
	requirements.NoError(err)
	requirements.NoError(store.CheckConversationPolicyAdmission(work))
	var profile string
	requirements.NoError(st.DB().QueryRowContext(ctx, `SELECT profile_id FROM document_extraction_heads LIMIT 1`).Scan(&profile))
	changed, err := st.RetireDocumentExtractionProfile(ctx, profile)
	requirements.NoError(err)
	requirements.True(changed)
	assert.ErrorIs(t, store.CheckConversationPolicyAdmission(work), store.ErrConversationPolicyStale)
}
