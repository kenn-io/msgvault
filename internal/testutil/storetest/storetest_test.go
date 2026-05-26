package storetest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFixtureNewMessage_UniqueIDs(t *testing.T) {
	f := New(t)
	m1 := f.NewMessage().Build()
	m2 := f.NewMessage().Build()
	assert.NotEqual(t, m1.SourceMessageID, m2.SourceMessageID, "expected unique IDs")
}

func TestFixtureNewMessage_DeterministicPerFixture(t *testing.T) {
	// Two fixtures should each start their counters at 1.
	f1 := New(t)
	f2 := New(t)
	m1 := f1.NewMessage().Build()
	m2 := f2.NewMessage().Build()
	assert.Equal(t, "fixture-msg-1", m1.SourceMessageID, "f1 first message ID")
	assert.Equal(t, "fixture-msg-1", m2.SourceMessageID, "f2 first message ID")
}

func TestMixedBuilders_NoDuplicateSourceMessageID(t *testing.T) {
	f := New(t)
	m1 := f.NewMessage().Build()
	m2 := NewMessage(f.Source.ID, f.ConvID).Build()
	assert.NotEqual(t, m1.SourceMessageID, m2.SourceMessageID, "mixed builders produced same SourceMessageID")
}

func TestNewMessage_Create(t *testing.T) {
	f := New(t)
	id := f.NewMessage().WithSubject("hello").Create(t, f.Store)
	assert.NotZero(t, id, "expected non-zero message ID")

	// Verify it's in the database.
	var count int
	err := f.Store.DB().QueryRow(f.Store.Rebind("SELECT COUNT(*) FROM messages WHERE id = ?"), id).Scan(&count)
	require.NoError(t, err, "query message")
	assert.Equal(t, 1, count, "message count")
}
