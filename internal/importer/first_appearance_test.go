package importer

import (
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestRelayedFromDoesNotIdentifyIntroducer(t *testing.T) {
	req := require.New(t)
	check := assert.New(t)
	f := storetest.New(t)
	req.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
	target := f.EnsureParticipant("target@example.test", "Target", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(target)
	req.NoError(err)
	at := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	raw := []byte("From: Author <notifications@github.com>\r\nTo: owner@example.test, target@example.test\r\nX-GitHub-Sender: example-author\r\nDate: Sat, 01 Aug 2026 12:00:00 +0000\r\nSubject: Notice\r\nMessage-ID: <relay@example.test>\r\n\r\nHello")
	req.NoError(IngestRawMessage(t.Context(), f.Store, f.Source.ID, "owner@example.test", "", nil, "relay", "relay", raw, at, slog.Default(), nil))
	var messageID int64
	req.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`), f.Source.ID, "relay").Scan(&messageID))
	projector, err := activity.NewProjector(f.Store, activity.Options{Timezone: "UTC", BatchSize: 10, MaxDirectCounterparts: 25})
	req.NoError(err)
	_, err = projector.RunOnce(t.Context())
	req.NoError(err)
	state, err := f.Store.ContactStateContext(t.Context(), person.ID, at)
	req.NoError(err)
	check.Equal(fmt.Sprintf("message:%d", messageID), state.FirstSeenRef)
	check.Equal(store.FirstContactObserved, state.FirstContactKind)
	check.Nil(state.Introducer)
}
