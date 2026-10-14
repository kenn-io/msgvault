package store

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestParseMeetingRefForms(t *testing.T) {
	assertions := assert.New(t)

	for _, tt := range []struct {
		ref, kind, key string
		id             int64
	}{
		{"evt:42", "evt", "", 42},
		{"evt:42@20270302T170000Z", "evt", "20270302T170000Z", 42},
		{"evt:42@20270314", "evt", "20270314", 42},
		{"msg:42", "msg", "", 42},
	} {
		t.Run(tt.ref, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			kind, occ, msg, err := ParseMeetingRef(tt.ref)
			requirements.NoError(err)
			assertions.Equal(tt.kind, kind)
			if kind == "evt" {
				assertions.Equal(OccurrenceRef{MessageID: tt.id, Key: tt.key}, occ)
				assertions.Zero(msg)
				assertions.Equal(tt.ref, FormatOccurrenceRef(occ))
			} else {
				assertions.Equal(tt.id, msg)
			}
		})
	}
	for _, ref := range []string{"", "evt:", "evt:42@", "evt:42@20270230", "evt:42@20270302T170000", "msg:42@20270314", "other:42", "evt:abc", "evt:42@20270314@20270315", "evt:+42", "evt: 42"} {
		_, _, _, err := ParseMeetingRef(ref)
		assertions.Error(err, ref)
	}
}

func TestCalendarEventSourceScope(t *testing.T) {
	assertions := assert.New(t)

	assertions.True(OwnerScope().Allows(42))
	assertions.False((SourceScope{}).Allows(42))
	assertions.True((SourceScope{IDs: []int64{42}}).Allows(42))
	assertions.False((SourceScope{IDs: []int64{-1}}).Allows(42))
}

func TestMeetingRefSignedArchiveIDs(t *testing.T) {
	for _, tt := range []struct {
		ref string
		id  int64
	}{{"evt:0", 0}, {"evt:-42", -42}, {"msg:-42", -42}} {
		t.Run(tt.ref, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			kind, occ, msg, err := ParseMeetingRef(tt.ref)
			requirements.NoError(err)
			if kind == "evt" {
				assertions.Equal(tt.id, occ.MessageID)
				assertions.Equal(tt.ref, FormatOccurrenceRef(occ))
			} else {
				assertions.Equal(tt.id, msg)
			}
		})
	}
}

func TestCalendarEventSourceScopeEmptyPopulation(t *testing.T) {
	assertions := assert.New(t)
	sc := SourceScope{IDs: []int64{-1}}
	assertions.False(sc.Allows(-1))
	assertions.False(sc.Allows(0))
	assertions.False(sc.Allows(42))
}
