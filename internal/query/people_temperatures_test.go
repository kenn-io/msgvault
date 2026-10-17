package query

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestPeopleTemperaturesPublishedCacheOnly(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	empty := &DuckDBEngine{}
	got, published, err := empty.PeopleTemperatures(t.Context(), map[int64][]int64{1: {1}})
	must.NoError(err)
	check.False(published)
	check.Empty(got)
	b := NewTestDataBuilder(t)
	source := b.AddSourceWithType("archive@example.net", "gmail")
	owner := b.AddParticipant("owner@example.net", "example.net", "Owner")
	first := b.AddParticipant("first@example.net", "example.net", "First")
	second := b.AddParticipant("second@example.net", "example.net", "Second")
	b.AddOwnerParticipant(source, owner)
	for i, recipient := range []int64{first, second, second} {
		msg := b.AddMessage(MessageOpt{
			SourceID: source, ConversationID: 1, SenderID: &owner,
			IsFromMe: true, SentAt: time.Now().UTC().Add(time.Duration(i) * time.Minute),
		})
		b.AddFrom(msg, owner, "Owner")
		b.AddTo(msg, recipient, "Recipient")
	}
	engine := b.BuildEngine()
	got, published, err = engine.PeopleTemperatures(t.Context(), map[int64][]int64{1: {first}, 2: {second}, 3: {first, second}})
	must.NoError(err)
	must.True(published)
	must.Contains(got, int64(1))
	must.Contains(got, int64(2))
	check.Less(got[1], got[2])
	check.Equal(max(got[1], got[2]), got[3])
}
