package store_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// draftFixture drives one provider's managed draft, created at revision 1, through the exported lifecycle.
type draftFixture struct {
	provider, sentinel, staleOutcome string
	claim                            func(rev int64, op string, raw []byte) error
	record                           func(rev int64, code string) error
	finish                           func(rev int64) error
	get                              func(id string) error
}

func newDraftFixtures(t *testing.T) []draftFixture {
	t.Helper()
	st, _, imap, _ := newReviewManagedDraft(t, "shared", 41, "old")
	ctx := t.Context()
	gsource, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(t, err)
	people := []store.ParticipantPersistData{{EmailAddress: "alice@example.com", Domain: "example.com"}}
	receipt := store.GmailDraftReceipt{SourceID: gsource.ID, GmailDraftID: "gmail-draft-1", GmailMessageID: "gmail-message-1", ThreadID: "thread-1"}
	gmail, err := st.PersistGmailDraftContext(ctx, receipt, people, gmailTestBuild(gsource.ID, 0, receipt, []byte("old")))
	require.NoError(t, err)
	return []draftFixture{{
		provider: "IMAP", sentinel: "IMAP", staleOutcome: "IMAP draft revision mismatch: expected 3, found 1",
		claim: func(rev int64, op string, raw []byte) error {
			_, err := st.ClaimIMAPDraftContext(ctx, imap.DraftID, rev, op, raw)
			return err
		},
		record: func(rev int64, code string) error {
			return st.RecordIMAPDraftOutcomeContext(ctx, imap.DraftID, rev, code, nil)
		},
		finish: func(rev int64) error {
			require.NoError(t, st.RecordIMAPDraftOutcomeContext(ctx, imap.DraftID, rev, store.IMAPDraftCodeRemoved, nil))
			_, err := st.FinishIMAPDraftRemovalContext(ctx, imap.DraftID, rev)
			return err
		},
		get: func(id string) error { _, err := st.GetIMAPDraftContext(ctx, id); return err },
	}, {
		provider: "Gmail", sentinel: "gmail", staleOutcome: "gmail draft revision mismatch",
		claim: func(rev int64, op string, raw []byte) error {
			_, err := st.ClaimGmailDraftContext(ctx, gmail.DraftID, rev, op, raw)
			return err
		},
		record: func(rev int64, code string) error {
			return st.RecordGmailDraftOutcomeContext(ctx, gmail.DraftID, rev, code, "")
		},
		finish: func(rev int64) error { _, err := st.FinishGmailDraftDeleteContext(ctx, gmail.DraftID, rev); return err },
		get:    func(id string) error { _, err := st.GetGmailDraftContext(ctx, id); return err },
	}}
}

// TestManagedDraftLifecycleErrorText pins the error text the shared lifecycle returns for each provider.
func TestManagedDraftLifecycleErrorText(t *testing.T) {
	require := require.New(t)
	for _, f := range newDraftFixtures(t) {
		require.EqualError(f.get("draft\rone"), "invalid "+f.provider+" draft ID")
		require.EqualError(f.get("draft-absent"), `draft "draft-absent": `+f.sentinel+" draft not found")
		require.EqualError(f.claim(0, "delete", nil), f.sentinel+" draft revision mismatch: expected positive revision")
		require.EqualError(f.claim(1, "archive", nil), "invalid "+f.provider+` draft state: unknown operation "archive"`)
		require.EqualError(f.claim(1, "edit", nil), "edit candidate must not be empty")
		require.EqualError(f.claim(7, "delete", nil), f.sentinel+" draft revision mismatch: expected 7, found 1")
		require.EqualError(f.record(1, " "), "invalid "+f.provider+" draft state: outcome requires positive revision and code")
		require.NoError(f.claim(1, "delete", nil))
		require.EqualError(f.claim(1, "delete", nil), f.sentinel+" draft has a pending operation")
		require.EqualError(f.record(3, "accepted"), f.staleOutcome)
		require.NoError(f.finish(1))
		require.EqualError(f.claim(2, "delete", nil), "invalid "+f.provider+" draft state: draft is discarded")
	}
}
