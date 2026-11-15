package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestCommitmentSourceDeletedBeforeExtraction(t *testing.T) {
	r, a, ctx := require.New(t), assert.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "commitment-evidence", false)
	st := f.store
	_, persist := newCommitmentTestConversation(t, f)
	profile := f.request.Generation.Policy.ProviderPolicyFingerprint
	_, err := st.RevokePersonInferenceConsent(ctx, profile, "test")
	r.NoError(err)
	_, _, err = st.GrantPersonCommitmentConsent(ctx, profile, "test")
	r.NoError(err)
	const quote = "I will send the draft."
	id := persist("promise", quote, -time.Hour)
	cfg, commitmentProfile := commitmentTestConfig()
	window, err := st.LoadPersonCommitmentWindow(ctx, f.personID, commitmentProfile, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	r.Len(window.Sources, 1)
	_, err = st.db.ExecContext(ctx, st.Rebind("UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?"), id)
	r.NoError(err)
	deleted, err := st.LoadPersonCommitmentWindow(ctx, f.personID, commitmentProfile, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	a.Empty(deleted.Sources)
	request := commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{
		Promises: []peoplesweep.CommitmentPromise{{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo, Quote: quote, Summary: "Send draft", End: int64(len(quote))}},
	})
	result, err := st.ApplyPersonSweep(ctx, request)
	r.NoError(err)
	a.Equal(peoplesweep.FailureArchiveGap, result.CommitmentFailureClass)
	claims, err := st.ListPersonFactClaimsContext(ctx, f.personID, personfacts.ClaimFilter{Limit: 10})
	r.NoError(err)
	a.Empty(claims)
}

func applyCommitmentTestSources(t *testing.T, f personSweepApplyFixture, cfg peoplesweep.CommitmentConfig) {
	t.Helper()
	r := require.New(t)
	_, profile := commitmentTestConfig()
	window, err := f.store.LoadPersonCommitmentWindow(t.Context(), f.personID, profile, cfg, personFactLedgerNow, "UTC")
	r.NoError(err)
	r.NotEmpty(window.Sources)
	output := peoplesweep.ParsedCommitments{}
	for _, source := range window.Sources {
		output.Promises = append(output.Promises, peoplesweep.CommitmentPromise{
			SourceID: source.ID, Direction: source.Direction, Quote: source.Text,
			Start: source.Start, End: source.Start + int64(len([]rune(source.Text))), Summary: "Keep the promise",
		})
	}
	_, err = f.store.ApplyPersonSweep(t.Context(), commitmentApplyRequest(t, f, window, cfg, output))
	r.NoError(err)
}
