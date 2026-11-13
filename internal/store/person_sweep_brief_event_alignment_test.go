package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
)

type eventReadErrorAligner struct{ err error }

func (a eventReadErrorAligner) Align(ctx context.Context, _ personfacts.EvidenceInput) (personfacts.AlignmentResult, error) {
	if ctx.Err() != nil {
		return personfacts.AlignmentResult{}, ctx.Err()
	}
	return personfacts.AlignmentResult{}, a.err
}

func TestPersonSweepEventAlignsEveryVersionOfRequiredSource(t *testing.T) {
	required := require.New(t)
	st, personID := newPersonFactLedgerStore(t)
	participant, err := st.EnsureParticipant("alice@example.com", "Alice", "example.com")
	required.NoError(err)
	source, err := st.GetOrCreateSource("slack", "event-version-fixture")
	required.NoError(err)
	conversation, err := st.EnsureConversationWithType(source.ID, "event-version-chat", "direct_chat", "Synthetic chat")
	required.NoError(err)
	message, err := st.UpsertMessage(&Message{SourceID: source.ID, SourceMessageID: "event-version",
		ConversationID: conversation, MessageType: "chat",
		SenderID: sql.NullInt64{Int64: participant, Valid: true},
		SentAt:   sql.NullTime{Time: personFactLedgerNow, Valid: true}})
	required.NoError(err)
	_, err = st.db.ExecContext(t.Context(), st.Rebind(`INSERT INTO message_bodies (message_id, body_text) VALUES (?, ?)`), message, "I move on Monday.")
	required.NoError(err)
	oldItems, err := st.HydratePersonSweepMessages(t.Context(), personID, []int64{message}, 0)
	required.NoError(err)
	required.Len(oldItems, 1)
	old, err := peoplesweep.PersonFactEvidenceInput(oldItems[0])
	required.NoError(err)
	_, err = st.db.ExecContext(t.Context(), st.Rebind(`UPDATE message_bodies SET body_text = ? WHERE message_id = ?`), "I move on Friday.", message)
	required.NoError(err)
	newItems, err := st.HydratePersonSweepMessages(t.Context(), personID, []int64{message}, 0)
	required.NoError(err)
	required.Len(newItems, 1)
	current, err := peoplesweep.PersonFactEvidenceInput(newItems[0])
	required.NoError(err)
	required.Equal(old.SourceRef, current.SourceRef)
	required.NotEqual(old.ContentSHA256, current.ContentSHA256)

	for _, tc := range []struct {
		name      string
		evidence  []personfacts.EvidenceInput
		wantError bool
	}{
		{"current then stale", []personfacts.EvidenceInput{current, old}, true},
		{"stale then current", []personfacts.EvidenceInput{old, current}, true},
		{"accepted duplicate", []personfacts.EvidenceInput{current, current}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			required := require.New(t)
			brief := briefApplyResult(personID, current.SourceRef)
			brief.Evidence = tc.evidence
			brief.Boundary.EventMemory = []peoplesweep.BriefEventMemory{{Sources: []peoplesweep.EvidenceRef{newItems[0].Ref}}}
			request := peoplesweep.ApplyRequest{Lease: peoplesweep.Lease{PersonID: personID}, Brief: brief}
			prepared, err := preparePersonSweepBriefEvidence(t.Context(), request, PersonSweepEvidenceAligner{Store: st})
			if tc.wantError {
				required.ErrorIs(err, peoplesweep.ErrBriefEventEvidenceChanged)
				required.ErrorIs(err, peoplesweep.ErrInvalidStructuredOutput)
				return
			}
			required.NoError(err)
			required.Len(prepared, 1)
			required.Equal(current.ContentSHA256, prepared[0].input.ContentSHA256)
		})
	}
}

func TestPersonSweepEventOperationalAlignmentFailureIsBriefOnly(t *testing.T) {
	require := require.New(t)
	ref := peoplesweep.EvidenceRef{SourceLane: peoplesweep.SourceConversationText,
		SourceID: 1, MessageID: 2, SourceMessageID: "synthetic", SpanEnd: 12}
	encoded, err := peoplesweep.EncodePersonSweepEvidenceRef(ref)
	require.NoError(err)
	brief := briefApplyResult(7, encoded)
	brief.Boundary.EventMemory = []peoplesweep.BriefEventMemory{{Sources: []peoplesweep.EvidenceRef{ref}}}
	request := peoplesweep.ApplyRequest{Lease: peoplesweep.Lease{PersonID: 7}, Brief: brief}
	cause := errors.New("historical event source could not be read")
	_, err = preparePersonSweepBriefEvidence(t.Context(), request, eventReadErrorAligner{err: cause})
	require.ErrorIs(err, cause)
	require.ErrorIs(err, peoplesweep.ErrBriefEventEvidenceChanged)
	require.NotErrorIs(err, peoplesweep.ErrInvalidStructuredOutput)
	_, err = preparePersonSweepBriefEvidence(t.Context(), request,
		&stubBriefAligner{rejectRefs: map[string]struct{}{encoded: {}}})
	require.ErrorIs(err, peoplesweep.ErrBriefEventEvidenceChanged)
	require.ErrorIs(err, peoplesweep.ErrInvalidStructuredOutput)
	_, err = preparePersonSweepBriefEvidence(t.Context(), request, eventReadErrorAligner{err: peoplesweep.ErrSourceTextUnavailable})
	require.ErrorIs(err, peoplesweep.ErrBriefEventEvidenceChanged)
	require.ErrorIs(err, peoplesweep.ErrInvalidStructuredOutput)
	withoutEvidence := *brief
	withoutEvidence.Evidence = nil
	request.Brief = &withoutEvidence
	_, err = preparePersonSweepBriefEvidence(t.Context(), request, &stubBriefAligner{})
	require.ErrorIs(err, peoplesweep.ErrBriefEventEvidenceChanged)
	require.ErrorIs(err, peoplesweep.ErrInvalidStructuredOutput)
	request.Brief = brief
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = preparePersonSweepBriefEvidence(ctx, request, eventReadErrorAligner{})
	require.ErrorIs(err, context.Canceled)
	require.NotErrorIs(err, peoplesweep.ErrBriefEventEvidenceChanged, "cancellation remains fatal")
}
