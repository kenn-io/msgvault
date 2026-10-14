package store

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func FuzzPersonMeetingReference(f *testing.F) {
	for _, id := range []int64{1, 7, 9007199254740991} {
		f.Add(id)
	}
	f.Fuzz(func(t *testing.T, id int64) {
		if id <= 0 || id > 9007199254740991 {
			return
		}
		for _, kind := range []string{"person", "participant"} {
			gotKind, gotID, err := ParsePersonMeetingRef(fmt.Sprintf("%s:%d", kind, id))
			require.NoError(t, err)
			assert.Equal(t, kind, gotKind)
			assert.Equal(t, id, gotID)
		}
	})
}

func TestPersonMeetingReferenceValidation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	for _, ref := range []string{"person:0", "participant:-1", "unknown:7", "person:1:2", "9007199254740992", " 7 ", "person:+1"} {
		_, _, err := ParsePersonMeetingRef(ref)
		requirements.ErrorIs(err, ErrInvalidArgument)
	}
	kind, id, err := ParsePersonMeetingRef("7")
	requirements.NoError(err)
	assertions.Equal("person", kind)
	assertions.Equal(int64(7), id)
}
