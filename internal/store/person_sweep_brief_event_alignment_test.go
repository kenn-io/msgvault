package store

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
)

type eventReadErrorAligner struct{}

func (eventReadErrorAligner) Align(ctx context.Context, _ personfacts.EvidenceInput) (personfacts.AlignmentResult, error) {
	if ctx.Err() != nil {
		return personfacts.AlignmentResult{}, ctx.Err()
	}
	return personfacts.AlignmentResult{}, errors.New("historical event source could not be read")
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
	_, err = preparePersonSweepBriefEvidence(t.Context(), request, eventReadErrorAligner{})
	require.ErrorIs(err, peoplesweep.ErrBriefEventEvidenceChanged)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = preparePersonSweepBriefEvidence(ctx, request, eventReadErrorAligner{})
	require.ErrorIs(err, context.Canceled)
	require.NotErrorIs(err, peoplesweep.ErrBriefEventEvidenceChanged, "cancellation remains fatal")
}
