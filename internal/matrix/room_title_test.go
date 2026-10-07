package matrix

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func TestImporterStripsTerminalControlsFromRoomTitles(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	named, present := roomTitle([]*event.Event{matrixTestEvent(t,
		`{"type":"m.room.name","event_id":"$name","state_key":"","content":{"name":"Team\u001b]0;pwned\u0007 room\u001b[2J\r\nnext"}}`)})
	assert.True(present)
	assert.Equal("Team room  next", named, "CR and LF become spaces; escape sequences and BEL are removed")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"next_batch":"next-1","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"events":[]}}}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/joined_members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"joined":{"@archive:example.org":{"display_name":"Archive"},"@member:example.org":{"display_name":"Mem\u001b[31mber\u0007"}}}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "token")
	require.NoError(err)
	st := testutil.NewTestStore(t)
	_, err = st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	_, err = NewImporter(st, &Runtime{Client: client}).Import(t.Context(), ImportOptions{UserID: "@archive:example.org"})
	require.NoError(err)
	var title string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT title FROM conversations WHERE source_conversation_id = ?`), "!room:example.org").Scan(&title))
	assert.Equal("Member", title)
}
