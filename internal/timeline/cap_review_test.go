package timeline

import (
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestBuildInvalidUTF8Snippet(t *testing.T) {
	testutil.SkipIfPostgres(t, "PostgreSQL rejects invalid UTF-8 before a snippet can be stored")
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
				assert.Contains(Markdown(got), "damaged \ufffd\ufffd")
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
