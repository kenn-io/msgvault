package store

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func TestCommitmentListRechecksEachCitationInSharedFulfilmentSource(t *testing.T) {
	r, a, ctx := require.New(t), assert.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "shared-fulfilment-source", false)
	st := f.store
	_, persist := newCommitmentTestConversation(t, f)
	persist("draft-promise", "I will send the draft.", -2*time.Hour)
	persist("venue-promise", "I will check the venue.", -time.Hour)
	cfg := peoplesweep.Config{}
	cfg.ApplyDefaults()
	cfg.Commitments.Enabled, cfg.Commitments.AutoFulfil = true, true
	applyCommitmentTestSources(t, f, cfg.Commitments)
	f = nextCommitmentTestAttempt(t, f, "-finish-initial")
	applyCommitmentTestSources(t, f, cfg.Commitments)
	promises, err := st.ListPersonCommitmentsContext(ctx, f.personID, f.request.CompletedAt, 5, false)
	r.NoError(err)
	r.Len(promises, 2)
	const first, second = "I sent the draft.", "I checked the venue."
	body := first + " " + second
	message := persist("shared-completion", body, -30*time.Minute)
	profile := peoplesweep.ProviderProfile{AllowSensitive: true, SourceSince: "2026-01-01",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}}
	apply := func(suffix string, fulfilments []peoplesweep.CommitmentFulfilment) {
		t.Helper()
		f = nextCommitmentTestAttempt(t, f, suffix)
		window, err := st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, f.request.Generation.ResolvedAt, "UTC")
		r.NoError(err)
		for i := range fulfilments {
			fulfilments[i].SourceID = commitmentSourceID(window, message)
		}
		result, err := st.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments,
			peoplesweep.ParsedCommitments{Promises: []peoplesweep.CommitmentPromise{}, Fulfilments: fulfilments}))
		r.NoError(err)
		r.Empty(result.CommitmentFailureClass)
	}
	venue := peoplesweep.CommitmentFulfilment{CommitmentID: promises[1].ID, Quote: second,
		Start: int64(len(first) + 1), End: int64(len(body)), Confirmed: true}
	apply("-both", []peoplesweep.CommitmentFulfilment{
		{CommitmentID: promises[0].ID, Quote: first, End: int64(len(first)), Confirmed: true}, venue,
	})
	open, err := st.ListPersonCommitmentsContext(ctx, f.personID, f.request.CompletedAt, 5, false)
	r.NoError(err)
	r.Empty(open)
	// An edit changes the whole-body version for both citations. Consented
	// revalidation then attaches only the still-valid venue span to the new
	// version; the old draft citation remains unsupported.
	_, err = st.db.ExecContext(ctx, st.Rebind("UPDATE message_bodies SET body_text = ? WHERE message_id = ?"),
		strings.Replace(body, "sent", "lost", 1), message)
	r.NoError(err)
	apply("-venue-revalidated", []peoplesweep.CommitmentFulfilment{venue})
	open, err = st.ListPersonCommitmentsContext(ctx, f.personID, f.request.CompletedAt, 5, false)
	r.NoError(err)
	r.Len(open, 1)
	a.Equal(promises[0].ID, open[0].ID)
	history, err := st.ListPersonCommitmentsContext(ctx, f.personID, f.request.CompletedAt, 5, true)
	r.NoError(err)
	r.Len(history, 2)
	for _, item := range history {
		want := CommitmentStateFulfilled
		if item.ID == promises[0].ID {
			want = LoopStateOpen
		}
		a.Equal(want, item.State, "citation for c%d must be checked independently", item.ID)
	}
}
