package peoplesweep_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestKnownEvidenceRequestValidate(t *testing.T) {
	tests := []struct {
		name    string
		request peoplesweep.KnownEvidenceRequest
		wantErr string
	}{
		{name: "person must be positive", request: peoplesweep.KnownEvidenceRequest{PersonID: 0, Limit: 1}, wantErr: "person id"},
		{name: "limit must be positive", request: peoplesweep.KnownEvidenceRequest{PersonID: 1, Limit: 0}, wantErr: "between 1 and 2000"},
		{name: "limit has a maximum", request: peoplesweep.KnownEvidenceRequest{PersonID: 1, Limit: peoplesweep.MaxKnownEvidenceLimit + 1}, wantErr: "between 1 and 2000"},
		{name: "kind requires key", request: peoplesweep.KnownEvidenceRequest{PersonID: 1, TargetKind: personfacts.TargetAttribute, Limit: 1}, wantErr: "both set or both empty"},
		{name: "key requires kind", request: peoplesweep.KnownEvidenceRequest{PersonID: 1, TargetKey: "city", Limit: 1}, wantErr: "both set or both empty"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.request.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.wantErr)
		})
	}
	for _, request := range []peoplesweep.KnownEvidenceRequest{
		{PersonID: 1, Limit: 1},
		{PersonID: 1, TargetKind: personfacts.TargetAttribute, TargetKey: "city", Limit: peoplesweep.MaxKnownEvidenceLimit},
	} {
		assert.NoError(t, request.Validate())
	}
}
