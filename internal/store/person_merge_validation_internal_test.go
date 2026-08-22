package store

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersonMergeRequestValidation(t *testing.T) {
	valid := PersonMergeRequest{
		SurvivorID:               1,
		AbsorbedID:               2,
		ExpectedSurvivorRevision: 3,
		ExpectedAbsorbedRevision: 4,
		IdempotencyKey:           "merge-1-2",
		Actor:                    "test",
	}
	tests := []struct {
		name   string
		mutate func(*PersonMergeRequest)
	}{
		{name: "survivor id", mutate: func(r *PersonMergeRequest) { r.SurvivorID = 0 }},
		{name: "absorbed id", mutate: func(r *PersonMergeRequest) { r.AbsorbedID = 0 }},
		{name: "same person", mutate: func(r *PersonMergeRequest) { r.AbsorbedID = r.SurvivorID }},
		{name: "survivor revision", mutate: func(r *PersonMergeRequest) { r.ExpectedSurvivorRevision = 0 }},
		{name: "absorbed revision", mutate: func(r *PersonMergeRequest) { r.ExpectedAbsorbedRevision = 0 }},
		{name: "empty key", mutate: func(r *PersonMergeRequest) { r.IdempotencyKey = " " }},
		{name: "oversized key", mutate: func(r *PersonMergeRequest) { r.IdempotencyKey = strings.Repeat("x", 129) }},
		{name: "empty actor", mutate: func(r *PersonMergeRequest) { r.Actor = " " }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := valid
			tc.mutate(&request)
			err := request.validate()
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrPersonMergeInvalid)
		})
	}
	require.NoError(t, valid.validate())
}

func TestPersonSplitRequestValidation(t *testing.T) {
	valid := PersonSplitRequest{
		SourcePersonID:         1,
		MergeID:                2,
		ParticipantIDs:         []int64{4, 3},
		ExpectedSourceRevision: 5,
		IdempotencyKey:         "split-2-3-4",
		Actor:                  "test",
	}
	tests := []struct {
		name   string
		mutate func(*PersonSplitRequest)
	}{
		{name: "source id", mutate: func(r *PersonSplitRequest) { r.SourcePersonID = 0 }},
		{name: "merge id", mutate: func(r *PersonSplitRequest) { r.MergeID = 0 }},
		{name: "empty participants", mutate: func(r *PersonSplitRequest) { r.ParticipantIDs = nil }},
		{name: "invalid participant", mutate: func(r *PersonSplitRequest) { r.ParticipantIDs = []int64{0} }},
		{name: "duplicate participant", mutate: func(r *PersonSplitRequest) { r.ParticipantIDs = []int64{3, 3} }},
		{name: "source revision", mutate: func(r *PersonSplitRequest) { r.ExpectedSourceRevision = 0 }},
		{name: "empty key", mutate: func(r *PersonSplitRequest) { r.IdempotencyKey = "" }},
		{name: "oversized key", mutate: func(r *PersonSplitRequest) { r.IdempotencyKey = strings.Repeat("x", 129) }},
		{name: "empty actor", mutate: func(r *PersonSplitRequest) { r.Actor = "" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := valid
			request.ParticipantIDs = append([]int64(nil), valid.ParticipantIDs...)
			tc.mutate(&request)
			err := request.validate()
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrPersonMergeInvalid)
		})
	}

	require.NoError(t, valid.validate())
	assert.Equal(t, []int64{3, 4}, valid.canonicalParticipantIDs())
	assert.Equal(t, []int64{4, 3}, valid.ParticipantIDs, "canonicalization must not mutate caller input")
}
