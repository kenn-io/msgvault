package store

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

// A format-valid date must not leave an active promise that its public readers
// cannot return or serialize. Rejection belongs to the optional lane.
func TestCommitmentExpiryRemainsRepresentableToReaders(t *testing.T) {
	for _, date := range []string{"2026-12-01", "9999-12-31"} {
		t.Run(date, func(t *testing.T) {
			r, a, ctx := require.New(t), assert.New(t), t.Context()
			f := newPersonSweepApplyFixture(t, "commitment-expiry-range", false)
			_, persist := newCommitmentTestConversation(t, f)
			const quote = "I will send the draft."
			persist("promise", quote, -time.Hour)
			cfg := peoplesweep.Config{}
			cfg.ApplyDefaults()
			cfg.Commitments.Enabled = true
			window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, peoplesweep.ProviderProfile{
				AllowSensitive: true, SourceSince: "2026-01-01", AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
			}, cfg.Commitments, personFactLedgerNow, "UTC")
			r.NoError(err)
			r.Len(window.Sources, 1)
			due := date
			request := commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{
				Promises: []peoplesweep.CommitmentPromise{{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo,
					Quote: quote, Summary: "Send the draft", End: int64(len(quote)), DueOn: &due}},
				Fulfilments: []peoplesweep.CommitmentFulfilment{},
			})
			result, err := f.store.ApplyPersonSweep(ctx, request)
			r.NoError(err)
			var stored int
			r.NoError(f.store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM person_commitments").Scan(&stored))
			if date == "2026-12-01" {
				r.Equal(1, stored, "normal due-date control must persist")
			}
			items, err := f.store.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 10, true)
			r.NoError(err)
			a.Equal(stored, len(items), "a newly persisted active promise must remain visible to its reader")
			_, err = json.Marshal(items)
			a.NoError(err, "public time fields must remain JSON representable")
			if stored == 0 {
				a.Equal(peoplesweep.FailureInvalidOutput, result.CommitmentFailureClass)
			}
		})
	}
}

// Two unsupported historical locators do not represent the new exact promise
// in an edited body. Refuse ambiguous output without consuming its frontier.
func TestCommitmentAmbiguousEditedPromiseDoesNotConsumeFrontier(t *testing.T) {
	r, a, ctx := require.New(t), assert.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "commitment-ambiguous-edit", false)
	st := f.store
	_, persist := newCommitmentTestConversation(t, f)
	const first, second = "I will send the report.", "I will book the venue."
	body := first + " " + second
	messageID := persist("two-promises", body, -time.Hour)
	cfg := peoplesweep.Config{}
	cfg.ApplyDefaults()
	cfg.Commitments.Enabled = true
	profile := peoplesweep.ProviderProfile{AllowSensitive: true, SourceSince: "2026-01-01", AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}}
	window, err := st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	r.Len(window.Sources, 1)
	_, err = st.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{
		Promises: []peoplesweep.CommitmentPromise{
			{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo, Quote: first, Summary: "Send report", End: int64(len(first))},
			{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo, Quote: second, Summary: "Book venue", Start: int64(len(first) + 1), End: int64(len(body))},
		}, Fulfilments: []peoplesweep.CommitmentFulfilment{},
	}))
	r.NoError(err)
	before, err := st.ListPersonCommitmentsContext(ctx, f.personID, f.request.CompletedAt, 10, true)
	r.NoError(err)
	r.Len(before, 2)
	// Complete the frozen anchor replay before admitting the later edit.
	f = nextCommitmentTestAttempt(t, f, "-finish-replay")
	window, err = st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	_, err = st.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{
		Promises: []peoplesweep.CommitmentPromise{}, Fulfilments: []peoplesweep.CommitmentFulfilment{},
	}))
	r.NoError(err)
	frontier := func() [5]int64 {
		var values [5]int64
		r.NoError(st.db.QueryRowContext(ctx, `SELECT through_sequence,backfill_message_id,anchor_after_id,anchor_through_sequence,anchor_backfill_id
 FROM person_commitment_frontiers WHERE person_id = ?`, f.personID).Scan(&values[0], &values[1], &values[2], &values[3], &values[4]))
		return values
	}
	previous := frontier()
	const edited = "I will send the revised report after the meeting."
	sequence, err := st.LatestPersonSweepChangeSequence(ctx)
	r.NoError(err)
	a.Equal(messageID, persist("two-promises", edited, -time.Hour))
	journal, err := st.ScanPersonSweepChanges(ctx, f.personID, sequence, 100)
	r.NoError(err)
	r.NotEmpty(journal)
	changes, err := st.BuildPersonSweepEvidenceStatusChanges(ctx, f.personID, journal)
	r.NoError(err)
	r.Len(changes, 2)
	f = nextCommitmentTestAttempt(t, f, "-edited")
	window, err = st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	r.Len(window.Sources, 1)
	request := commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{
		Promises: []peoplesweep.CommitmentPromise{{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo,
			Quote: edited, Summary: "Send revised report", End: int64(len(edited))}},
		Fulfilments: []peoplesweep.CommitmentFulfilment{},
	})
	request.Generation.ResolvedAt = request.Generation.ResolvedAt.Add(time.Minute)
	request.Generation.EvidenceStatusChanges = changes
	result, err := st.ApplyPersonSweep(ctx, request)
	r.NoError(err)
	a.Equal(peoplesweep.FailureInvalidOutput, result.CommitmentFailureClass)
	a.Equal(previous, frontier(), "unrepresented ambiguous output cannot consume source or anchor progress")
	items, err := st.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 10, true)
	r.NoError(err)
	a.Empty(items, "the old unsupported citations cannot stand in for the edited promise")
}
