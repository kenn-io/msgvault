package query

import (
	"context"
	"testing"
	"time"

	"go.kenn.io/msgvault/internal/testutil/tbmock"
)

func TestAddLabel_ValidName(t *testing.T) {
	b := NewTestDataBuilder(t)
	id := b.AddLabel("INBOX")
	if id != 1 {
		t.Errorf("expected label ID 1, got %d", id)
	}
	id2 := b.AddLabel("SENT")
	if id2 != 2 {
		t.Errorf("expected label ID 2, got %d", id2)
	}
}

func TestAddMessage_ExplicitSourceID_BypassesCheck(t *testing.T) {
	// Explicit SourceID bypasses the "no sources" check.
	b := NewTestDataBuilder(t)
	id := b.AddMessage(MessageOpt{
		Subject:  "test",
		SourceID: 99, // explicit, so no sources needed
	})
	if id != 1 {
		t.Errorf("expected message ID 1, got %d", id)
	}
}

func TestTestDataBuilder_ValidationFailures(t *testing.T) {
	tests := []struct {
		name string
		fn   func(*TestDataBuilder)
	}{
		{
			name: "AddMessage_WithoutSources",
			fn:   func(b *TestDataBuilder) { b.AddMessage(MessageOpt{Subject: "fail"}) },
		},
		{
			name: "AddAttachment_MissingMessage",
			fn: func(b *TestDataBuilder) {
				b.AddSource("a@test.com")
				b.AddMessage(MessageOpt{Subject: "ok"})
				b.AddAttachment(999, 1024, "missing.txt")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mtb := tbmock.NewMockTB(t)
			tbmock.ExpectFatal(mtb, func() {
				b := NewTestDataBuilder(mtb)
				tc.fn(b)
			})
			if !mtb.Failed() {
				t.Error("expected builder to fail")
			}
		})
	}
}

func TestAddMessage_UsesFirstSource(t *testing.T) {
	b := NewTestDataBuilder(t)
	srcID := b.AddSource("a@test.com")
	b.AddSource("b@test.com") // Add a second source to ensure first is selected
	msgID := b.AddMessage(MessageOpt{Subject: "test"})
	if msgID != 1 {
		t.Errorf("expected message ID 1, got %d", msgID)
	}

	// Verify the message uses the first source ID (not the second)
	if len(b.messages) != 1 {
		t.Fatalf("expected 1 message in builder, got %d", len(b.messages))
	}
	if b.messages[0].SourceID != srcID {
		t.Errorf("expected message to use first source ID %d, got %d", srcID, b.messages[0].SourceID)
	}

	// Also verify through the engine that the data is correctly built
	engine := b.BuildEngine()
	defer func() { _ = engine.Close() }()

	stats, err := engine.GetTotalStats(context.Background(), StatsOptions{})
	if err != nil {
		t.Fatalf("GetTotalStats: %v", err)
	}
	if stats.MessageCount != 1 {
		t.Errorf("expected 1 message, got %d", stats.MessageCount)
	}
}

func TestAddAttachment_SetsHasAttachments(t *testing.T) {
	b := NewTestDataBuilder(t)
	b.AddSource("a@test.com")
	msgID := b.AddMessage(MessageOpt{Subject: "with attachment"})

	if b.messages[0].HasAttachments {
		t.Error("HasAttachments should be false before AddAttachment")
	}

	b.AddAttachment(msgID, 1024, "file.txt")

	if !b.messages[0].HasAttachments {
		t.Error("HasAttachments should be true after AddAttachment")
	}
}

func TestBuild_EmptyAuxiliaryTables(t *testing.T) {
	// Build should succeed with messages but no participants, labels, etc.
	b := NewTestDataBuilder(t)
	b.AddSource("a@test.com")
	b.AddMessage(MessageOpt{
		Subject: "solo message",
		SentAt:  time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC),
	})

	engine := b.BuildEngine()
	defer func() { _ = engine.Close() }()

	// Should be able to query without errors.
	stats, err := engine.GetTotalStats(context.Background(), StatsOptions{})
	if err != nil {
		t.Fatalf("GetTotalStats: %v", err)
	}
	if stats.MessageCount != 1 {
		t.Errorf("expected 1 message, got %d", stats.MessageCount)
	}
}
