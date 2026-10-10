package identitycontrol

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPreviewRequestRequiresOneExactIdentityPair(t *testing.T) {
	const maxSafeID = int64(9_007_199_254_740_991)
	for _, tt := range []struct {
		name    string
		request PreviewRequest
		valid   bool
	}{
		{"graph link", PreviewRequest{Operation: OperationGraphLink, Target: IdentityTarget{ParticipantID: 1, OtherParticipantID: 2}}, true},
		{"graph unlink", PreviewRequest{Operation: OperationGraphUnlink, Target: IdentityTarget{ParticipantID: 1, OtherParticipantID: 2}}, true},
		{"person link", PreviewRequest{Operation: OperationPersonLink, Target: IdentityTarget{ParticipantID: 1, PersonID: 3}}, true},
		{"person unlink", PreviewRequest{Operation: OperationPersonUnlink, Target: IdentityTarget{ParticipantID: 1, PersonID: 3}}, true},
		{"same numeric ID across kinds for link", PreviewRequest{Operation: OperationPersonLink, Target: IdentityTarget{ParticipantID: 1, PersonID: 1}}, true},
		{"same numeric ID across kinds for unlink", PreviewRequest{Operation: OperationPersonUnlink, Target: IdentityTarget{ParticipantID: 1, PersonID: 1}}, true},
		{"largest exact JSON ID", PreviewRequest{Operation: OperationGraphLink, Target: IdentityTarget{ParticipantID: maxSafeID - 1, OtherParticipantID: maxSafeID}}, true},
		{"missing operation", PreviewRequest{Target: IdentityTarget{ParticipantID: 1, OtherParticipantID: 2}}, false},
		{"unknown operation", PreviewRequest{Operation: "merge-conversation", Target: IdentityTarget{ParticipantID: 1, OtherParticipantID: 2}}, false},
		{"missing first participant", PreviewRequest{Operation: OperationGraphLink, Target: IdentityTarget{OtherParticipantID: 2}}, false},
		{"negative participant", PreviewRequest{Operation: OperationGraphLink, Target: IdentityTarget{ParticipantID: -1, OtherParticipantID: 2}}, false},
		{"missing graph endpoint", PreviewRequest{Operation: OperationGraphLink, Target: IdentityTarget{ParticipantID: 1}}, false},
		{"self graph edge", PreviewRequest{Operation: OperationGraphLink, Target: IdentityTarget{ParticipantID: 1, OtherParticipantID: 1}}, false},
		{"ambiguous graph and person", PreviewRequest{Operation: OperationGraphLink, Target: IdentityTarget{ParticipantID: 1, OtherParticipantID: 2, PersonID: 3}}, false},
		{"person operation with graph pair", PreviewRequest{Operation: OperationPersonLink, Target: IdentityTarget{ParticipantID: 1, OtherParticipantID: 2}}, false},
		{"person operation with extra graph endpoint", PreviewRequest{Operation: OperationPersonUnlink, Target: IdentityTarget{ParticipantID: 1, OtherParticipantID: 2, PersonID: 3}}, false},
		{"missing person", PreviewRequest{Operation: OperationPersonLink, Target: IdentityTarget{ParticipantID: 1}}, false},
		{"negative person", PreviewRequest{Operation: OperationPersonUnlink, Target: IdentityTarget{ParticipantID: 1, PersonID: -3}}, false},
		{"missing binding participant", PreviewRequest{Operation: OperationPersonLink, Target: IdentityTarget{PersonID: 3}}, false},
		{"rounded participant", PreviewRequest{Operation: OperationGraphLink, Target: IdentityTarget{ParticipantID: maxSafeID + 1, OtherParticipantID: 2}}, false},
		{"rounded other participant", PreviewRequest{Operation: OperationGraphUnlink, Target: IdentityTarget{ParticipantID: 1, OtherParticipantID: maxSafeID + 1}}, false},
		{"rounded person", PreviewRequest{Operation: OperationPersonLink, Target: IdentityTarget{ParticipantID: 1, PersonID: maxSafeID + 1}}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			requirements := require.New(t)
			before := tt.request
			err := tt.request.Validate()
			if tt.valid {
				requirements.NoError(err)
			} else {
				requirements.ErrorIs(err, ErrInvalidRequest)
			}
			assert.Equal(t, before, tt.request, "validation must preserve caller input")
		})
	}
}

func TestCanonicalIdentityPairPreservesBindingDirection(t *testing.T) {
	for _, tt := range []struct {
		name      string
		operation Operation
		target    IdentityTarget
		want      IdentityTarget
	}{
		{"graph link orders participant pair", OperationGraphLink, IdentityTarget{ParticipantID: 7, OtherParticipantID: 2}, IdentityTarget{ParticipantID: 2, OtherParticipantID: 7}},
		{"graph unlink orders participant pair", OperationGraphUnlink, IdentityTarget{ParticipantID: 7, OtherParticipantID: 2}, IdentityTarget{ParticipantID: 2, OtherParticipantID: 7}},
		{"person link retains participant and person", OperationPersonLink, IdentityTarget{ParticipantID: 7, PersonID: 2}, IdentityTarget{ParticipantID: 7, PersonID: 2}},
		{"person unlink retains participant and person", OperationPersonUnlink, IdentityTarget{ParticipantID: 7, PersonID: 2}, IdentityTarget{ParticipantID: 7, PersonID: 2}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := tt.target
			got := tt.target.Canonical(tt.operation)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, before, tt.target, "canonicalization must preserve caller input")
			assert.Equal(t, got, got.Canonical(tt.operation), "canonicalization is idempotent")
		})
	}
}
