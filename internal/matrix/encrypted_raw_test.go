package matrix

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func TestImporterRetainsCiphertextBesideDecryptedRaw(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := newKeyBackupFixture(t)
	timeline := fixture.encryptedEventJSON(t, "$sealed", "decrypted body")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
		// /sync timeline events carry no room_id; decryption must still
		// find the room's Megolm session on the first attempt.
		_, _ = fmt.Fprintf(w, `{"next_batch":"next-1","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"events":[%s]}}}}}`, timeline)
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/joined_members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"joined":{"@archive:example.org":{"display_name":"Archive"},"@sender:example.org":{"display_name":"Sender"}}}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, machine := newArchiveMachine(t, server.URL)
	fixture.shareWith(t, machine)
	decryptCalls := 0
	runtime := &Runtime{Client: client, machine: machine, decryptEvent: func(ctx context.Context, evt *event.Event) (*event.Event, error) {
		decryptCalls++
		return machine.DecryptMegolmEvent(ctx, evt)
	}}

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	_, err = NewImporter(st, runtime).Import(t.Context(), ImportOptions{UserID: source.Identifier})
	require.NoError(err)

	assert.Equal(1, decryptCalls)
	messages, err := st.MessageExistsBatch(source.ID, []string{"$sealed"})
	require.NoError(err)
	require.NotZero(messages["$sealed"])
	messageRaw, err := st.GetMessageRaw(messages["$sealed"])
	require.NoError(err)
	var decrypted event.Event
	require.NoError(json.Unmarshal(messageRaw, &decrypted))
	assert.Equal(event.EventMessage, decrypted.Type)
	ciphertext, roomID, err := st.MatrixEncryptedEvent(source.ID, "$sealed")
	require.NoError(err)
	assert.Equal(fixture.roomID.String(), roomID)
	var original event.Event
	require.NoError(json.Unmarshal(ciphertext, &original))
	assert.Equal(event.EventEncrypted, original.Type)
	assert.Equal(id.EventID("$sealed"), original.ID)
}
