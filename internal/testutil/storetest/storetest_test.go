package storetest

import (
	"testing"

	"go.kenn.io/msgvault/internal/testutil"
)

func TestFixtureNewMessage_UniqueIDs(t *testing.T) {
	f := New(t)
	m1 := f.NewMessage().Build()
	m2 := f.NewMessage().Build()
	if m1.SourceMessageID == m2.SourceMessageID {
		t.Errorf("expected unique IDs, both got %q", m1.SourceMessageID)
	}
}

func TestFixtureNewMessage_DeterministicPerFixture(t *testing.T) {
	// Two fixtures should each start their counters at 1.
	f1 := New(t)
	f2 := New(t)
	m1 := f1.NewMessage().Build()
	m2 := f2.NewMessage().Build()
	if m1.SourceMessageID != "fixture-msg-1" {
		t.Errorf("f1 first message ID = %q, want fixture-msg-1", m1.SourceMessageID)
	}
	if m2.SourceMessageID != "fixture-msg-1" {
		t.Errorf("f2 first message ID = %q, want fixture-msg-1", m2.SourceMessageID)
	}
}

func TestMixedBuilders_NoDuplicateSourceMessageID(t *testing.T) {
	f := New(t)
	m1 := f.NewMessage().Build()
	m2 := NewMessage(f.Source.ID, f.ConvID).Build()
	if m1.SourceMessageID == m2.SourceMessageID {
		t.Errorf("mixed builders produced same SourceMessageID: %q", m1.SourceMessageID)
	}
}

func TestNewMessage_Create(t *testing.T) {
	f := New(t)
	id := f.NewMessage().WithSubject("hello").Create(t, f.Store)
	if id == 0 {
		t.Error("expected non-zero message ID")
	}

	// Verify it's in the database.
	var count int
	err := f.Store.DB().QueryRow(f.Store.Rebind("SELECT COUNT(*) FROM messages WHERE id = ?"), id).Scan(&count)
	testutil.MustNoErr(t, err, "query message")
	if count != 1 {
		t.Errorf("message count = %d, want 1", count)
	}
}
