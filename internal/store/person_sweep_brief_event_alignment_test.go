package store

import (
	"context"
	"encoding/json/jsontext"
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

func TestPersonSweepRetiredCurrentEventStillRequiresSourceAlignment(t *testing.T) {
	require := require.New(t)
	ref := peoplesweep.EvidenceRef{SourceLane: peoplesweep.SourceConversationText,
		SourceID: 1, MessageID: 2, SourceMessageID: "synthetic", SpanEnd: 12}
	resolutionRef := peoplesweep.EvidenceRef{SourceLane: peoplesweep.SourceConversationText,
		SourceID: 3, MessageID: 4, SourceMessageID: "synthetic-resolution", SpanEnd: 18}
	encoded, err := peoplesweep.EncodePersonSweepEvidenceRef(ref)
	require.NoError(err)
	resolutionEncoded, err := peoplesweep.EncodePersonSweepEvidenceRef(resolutionRef)
	require.NoError(err)
	brief := briefApplyResult(7, encoded, resolutionEncoded)
	brief.Structured = jsontext.Value(`{"last_meaningful_interaction":null,"highlights":[{"text":"the move was cancelled","speaker":"person","evidence_ids":["event-anchor","event-resolution"],"observed_at":null,"confidence_basis_points":900,"life_event":{"date":"2026-09-10","anchor_id":"event-anchor","quote":"I cancelled the move.","question":"How did the move go?","resolution":{"evidence_id":"event-resolution","quote":"I cancelled the move."},"state":"resolved"}}],"follow_ups":[],"appreciations":[],"uncertainties":[],"possible_attributes":[]}`)
	brief.CurrentEventEvidenceRefs = []peoplesweep.EvidenceRef{ref, resolutionRef}
	// The resolved event was retired from future memory for packet capacity;
	// its current output still requires the source to align before storage.
	request := peoplesweep.ApplyRequest{Lease: peoplesweep.Lease{PersonID: 7}, Brief: brief}
	_, err = preparePersonSweepBriefEvidence(t.Context(), request, eventReadErrorAligner{})
	require.ErrorIs(err, peoplesweep.ErrBriefEventEvidenceChanged)
}
