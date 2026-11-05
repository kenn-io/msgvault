package timeline

import (
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestBuildInvalidUTF8Snippet(t *testing.T) {
	f := newTimelineFixture(t)
	f.add(f.now, false, func(m *store.Message) { m.Snippet = sql.NullString{String: "damaged \xff\xfe", Valid: true} })
	f.project()
	for _, format := range []string{"json", "markdown"} {
		t.Run(format, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			req := dayRequest()
			req.Format = format
			got, err := Build(t.Context(), f.src, req)
			require.NoError(err)
			entries := allDayEntries(got)
			require.Len(entries, 1)
			if format == "json" {
				wire, err := json.Marshal(got, jsontext.AllowInvalidUTF8(true))
				require.NoError(err)
				assert.Contains(string(wire), "damaged \ufffd\ufffd")
			} else {
				assert.Contains(Markdown(got), "damaged")
			}
		})
	}
}

func TestAnchorColdConversationWithIncludeCold(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newTimelineFixture(t)
	conv, err := f.Store.EnsureConversation(f.Source.ID, "cold-anchor", "Cold conversation")
	require.NoError(err)
	require.NoError(f.seedConversationPolicy(t.Context(), f.Source.ID, "cold-anchor"))
	id := f.add(f.now, false, func(m *store.Message) { m.ConversationID = conv })
	f.project()
	req := anchorRequest(messageRef(id))
	req.IncludeCold = true
	message, err := Build(t.Context(), f.src, req)
	require.NoError(err)
	req.Around = fmt.Sprintf("conv:%d", conv)
	conversation, err := Build(t.Context(), f.src, req)
	require.NoError(err)
	assert.Equal(message.Anchor.Time, conversation.Anchor.Time)
	assert.Equal(message.History, conversation.History)
}

func TestRelationshipHistoryBeyondBurstCap(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	testutil.SkipIfPostgres(t, "large burst query exceeds the timeline read deadline on PostgreSQL")
	f := newTimelineFixture(t)
	var oldest int64
	var cutoff Entry
	for i := range 5001 {
		conv, err := f.Store.EnsureConversation(f.Source.ID, fmt.Sprintf("history-%d", i), "Synthetic conversation")
		require.NoError(err)
		at := f.now.Add(-time.Duration(i) * time.Second)
		oldest = f.add(at, false, func(m *store.Message) { m.ConversationID = conv })
		if i == 4999 {
			cutoff = Entry{Kind: "burst", Key: fmt.Sprintf("burst:%d:%d:2026-10-22", f.Source.ID, conv), Time: at}
		}
	}
	f.project()
	f.src.Loops, f.src.Inventory = nil, nil
	req := Request{With: "email:alice@example.com", Mode: ModeRelationship, Format: "json", Budget: 65536, Scope: Scope{Owner: true}}
	// Resume after the 5,000th burst without rebuilding the preceding 250 pages.
	var err error
	req.Cursor, err = EncodeCursor(CursorBinding{ArchiveUID: f.src.ArchiveUID, Request: req, Window: Window{From: "2026-10-21", To: "2026-10-22"}, Timezone: f.src.Location.String(), Now: f.now}, map[string]string{"history": historyKey(cutoff)})
	require.NoError(err)
	require.NotEmpty(req.Cursor)
	got, err := Build(t.Context(), f.src, req)
	require.NoError(err)
	require.Len(got.History, 1)
	assert.Equal([]int64{oldest}, got.History[0].Burst.LatestMessageIDs)
	assert.Empty(got.Cursor)
}
