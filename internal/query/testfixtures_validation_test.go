package query

import (
	"context"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/tbmock"
)

func TestAddLabel_ValidName(t *testing.T) {
	b := NewTestDataBuilder(t)
	id := b.AddLabel("INBOX")
	assertpkg.Equal(t, int64(1), id)
	id2 := b.AddLabel("SENT")
	assertpkg.Equal(t, int64(2), id2)
}

func TestAddMessage_ExplicitSourceID_BypassesCheck(t *testing.T) {
	// Explicit SourceID bypasses the "no sources" check.
	b := NewTestDataBuilder(t)
	id := b.AddMessage(MessageOpt{
		Subject:  "test",
		SourceID: 99, // explicit, so no sources needed
	})
	assertpkg.Equal(t, int64(1), id)
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
			assertpkg.True(t, mtb.Failed(), "expected builder to fail")
		})
	}
}

func TestAddMessage_UsesFirstSource(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	b := NewTestDataBuilder(t)
	srcID := b.AddSource("a@test.com")
	b.AddSource("b@test.com") // Add a second source to ensure first is selected
	msgID := b.AddMessage(MessageOpt{Subject: "test"})
	assert.Equal(int64(1), msgID)

	// Verify the message uses the first source ID (not the second)
	require.Len(b.messages, 1, "messages in builder")
	assert.Equal(srcID, b.messages[0].SourceID, "message should use first source ID")

	// Also verify through the engine that the data is correctly built
	engine := b.BuildEngine()
	defer func() { _ = engine.Close() }()

	stats, err := engine.GetTotalStats(context.Background(), StatsOptions{})
	require.NoError(err, "GetTotalStats")
	assert.Equal(int64(1), stats.MessageCount)
}

func TestAddAttachment_SetsHasAttachments(t *testing.T) {
	b := NewTestDataBuilder(t)
	b.AddSource("a@test.com")
	msgID := b.AddMessage(MessageOpt{Subject: "with attachment"})

	assertpkg.False(t, b.messages[0].HasAttachments, "HasAttachments should be false before AddAttachment")

	b.AddAttachment(msgID, 1024, "file.txt")

	assertpkg.True(t, b.messages[0].HasAttachments, "HasAttachments should be true after AddAttachment")
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
	requirepkg.NoError(t, err, "GetTotalStats")
	assertpkg.Equal(t, int64(1), stats.MessageCount)
}
