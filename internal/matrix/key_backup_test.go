package matrix

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/crypto/backup"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type keyBackupFixture struct {
	roomID         id.RoomID
	sender         *crypto.OlmMachine
	session        backup.MegolmSessionData
	sessionID      id.SessionID
	outboundKey    string
	backupKey      *backup.MegolmBackupKey
	sessionFetches atomic.Int64
	versionFetches atomic.Int64
}

func newKeyBackupFixture(t *testing.T) *keyBackupFixture {
	t.Helper()
	require := require.New(t)
	ctx := t.Context()
	roomID := id.RoomID("!room:example.org")
	senderClient, err := mautrix.NewClient("https://example.invalid", "@sender:example.org", "token")
	require.NoError(err)
	senderClient.DeviceID = "SENDER"
	senderStore := crypto.NewMemoryStore(nil)
	sender := crypto.NewOlmMachine(senderClient, nil, senderStore, matrixCryptoTestStateStore{})
	require.NoError(sender.Load(ctx))
	outbound, err := crypto.NewOutboundGroupSession(roomID, &event.EncryptionEventContent{},
		&event.HistoryVisibilityEventContent{HistoryVisibility: event.HistoryVisibilityShared})
	require.NoError(err)
	outbound.Shared = true
	require.NoError(senderStore.AddOutboundGroupSession(ctx, outbound))
	identity := sender.OwnIdentity()
	inbound, err := crypto.NewInboundGroupSession(identity.IdentityKey, identity.SigningKey, roomID,
		outbound.Internal.Key(), 0, 0, outbound.SharedHistory, false)
	require.NoError(err)
	exported, err := inbound.Internal.Export(0)
	require.NoError(err)
	backupKey, err := backup.NewMegolmBackupKey()
	require.NoError(err)
	return &keyBackupFixture{
		roomID: roomID, sender: sender, sessionID: outbound.ID(), backupKey: backupKey,
		outboundKey: outbound.Internal.Key(),
		session: backup.MegolmSessionData{
			Algorithm: id.AlgorithmMegolmV1, SenderKey: identity.IdentityKey,
			SenderClaimedKeys: backup.SenderClaimedKeys{Ed25519: identity.SigningKey},
			SessionKey:        string(exported),
		},
	}
}

// handle serves the latest backup version and the one backed-up session, as a
// homeserver does after another device uploads a new room key.
func (f *keyBackupFixture) handle(t *testing.T, mux *http.ServeMux) {
	t.Helper()
	mux.HandleFunc("GET /_matrix/client/v3/room_keys/version", func(w http.ResponseWriter, _ *http.Request) {
		f.versionFetches.Add(1)
		publicKey := base64.RawStdEncoding.EncodeToString(f.backupKey.PublicKey().Bytes())
		_, _ = fmt.Fprintf(w, `{"algorithm":"m.megolm_backup.v1.curve25519-aes-sha2","auth_data":{"public_key":%q},"count":1,"etag":"1","version":"1"}`, publicKey)
	})
	mux.HandleFunc("GET /_matrix/client/v3/room_keys/keys/{room}/{session}", func(w http.ResponseWriter, r *http.Request) {
		f.sessionFetches.Add(1)
		if r.PathValue("session") != f.sessionID.String() || r.URL.Query().Get("version") != "1" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errcode":"M_NOT_FOUND","error":"No room_keys found"}`))
			return
		}
		encrypted, err := backup.EncryptSessionData(f.backupKey, f.session)
		if !assert.NoError(t, err) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		body, err := json.Marshal(mautrix.RespKeyBackupData[*backup.EncryptedSessionData[backup.MegolmSessionData]]{SessionData: encrypted})
		if !assert.NoError(t, err) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(body)
	})
}

// shareWith gives machine the room key directly, as a to-device share would.
func (f *keyBackupFixture) shareWith(t *testing.T, machine *crypto.OlmMachine) {
	t.Helper()
	inbound, err := crypto.NewInboundGroupSession(f.session.SenderKey, f.session.SenderClaimedKeys.Ed25519,
		f.roomID, f.outboundKey, 0, 0, f.session.SharedHistory, false)
	require.NoError(t, err)
	require.NoError(t, machine.CryptoStore.PutGroupSession(t.Context(), inbound))
}

func (f *keyBackupFixture) encryptedEventJSON(t *testing.T, eventID, body string) string {
	t.Helper()
	content, err := f.sender.EncryptMegolmEvent(t.Context(), f.roomID, event.EventMessage,
		&event.MessageEventContent{MsgType: event.MsgText, Body: body})
	require.NoError(t, err)
	encoded, err := json.Marshal(content)
	require.NoError(t, err)
	return fmt.Sprintf(`{"type":"m.room.encrypted","event_id":%q,"sender":"@sender:example.org","origin_server_ts":1000,"content":%s}`, eventID, encoded)
}

func newArchiveMachine(t *testing.T, homeserver string) (*mautrix.Client, *crypto.OlmMachine) {
	t.Helper()
	client, err := mautrix.NewClient(homeserver, "@archive:example.org", "token")
	require.NoError(t, err)
	client.DeviceID = "ARCHIVE"
	machine := crypto.NewOlmMachine(client, nil, crypto.NewMemoryStore(nil), matrixCryptoTestStateStore{})
	require.NoError(t, machine.Load(t.Context()))
	return client, machine
}

func TestSyncFetchesNewBackupKeyForPendingPlaceholder(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := newKeyBackupFixture(t)
	timeline := fixture.encryptedEventJSON(t, "$backed-up", "from backup")
	mux := http.NewServeMux()
	fixture.handle(t, mux)
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
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
	// add-matrix caches only the backup decryption key, never the recovery key.
	require.NoError(machine.CryptoStore.PutSecret(t.Context(), cachedBackupKeySecret,
		base64.RawStdEncoding.EncodeToString(fixture.backupKey.Bytes())))

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	runtime := &Runtime{Client: client, machine: machine, decryptEvent: machine.DecryptMegolmEvent}
	summary, err := NewImporter(st, runtime).Import(t.Context(), ImportOptions{UserID: source.Identifier})
	require.NoError(err)

	assert.Equal(int64(1), summary.UndecryptableRecovered)
	assert.Equal(int64(1), fixture.sessionFetches.Load())
	messages, err := st.MessageExistsBatch(source.ID, []string{"$backed-up"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(messages["$backed-up"])
	require.NoError(err)
	assert.Equal("from backup", body)
	ciphertext, _, err := st.MatrixEncryptedEvent(source.ID, "$backed-up")
	require.NoError(err)
	var original event.Event
	require.NoError(json.Unmarshal(ciphertext, &original))
	assert.Equal(event.EventEncrypted, original.Type, "recovery keeps the placeholder's ciphertext")
}

func TestSyncLogsBackupFailureWithoutProgress(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := newKeyBackupFixture(t)
	timeline := fixture.encryptedEventJSON(t, "$backed-up", "from backup")
	mux := http.NewServeMux()
	fixture.handle(t, mux)
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
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
	otherKey, err := backup.NewMegolmBackupKey()
	require.NoError(err)
	require.NoError(machine.CryptoStore.PutSecret(t.Context(), cachedBackupKeySecret,
		base64.RawStdEncoding.EncodeToString(otherKey.Bytes())))
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	runtime := &Runtime{Client: client, machine: machine, decryptEvent: machine.DecryptMegolmEvent}
	summary, err := NewImporter(st, runtime).Import(t.Context(), ImportOptions{UserID: source.Identifier})

	require.NoError(err, "a backup failure does not fail the sync")
	assert.Zero(summary.UndecryptableRecovered)
	assert.Contains(logs.String(), "matrix key backup unavailable")
}

func TestRestoreKeyBackupCachesBackupKeyForLaterSyncs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := newKeyBackupFixture(t)
	mux := http.NewServeMux()
	fixture.handle(t, mux)
	mux.HandleFunc("GET /_matrix/client/v3/room_keys/keys", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"rooms":{}}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, machine := newArchiveMachine(t, server.URL)
	runtime := &Runtime{Client: client, machine: machine}

	require.NoError(runtime.restoreWithBackupKey(t.Context(), fixture.backupKey))

	cached, err := machine.CryptoStore.GetSecret(t.Context(), cachedBackupKeySecret)
	require.NoError(err)
	assert.Equal(base64.RawStdEncoding.EncodeToString(fixture.backupKey.Bytes()), cached)
	// A new runtime (the next sync) uses the cached key to fetch a session
	// that another device added after registration.
	later := &Runtime{Client: client, machine: machine}
	encrypted := matrixTestEvent(t, fixture.encryptedEventJSON(t, "$later", "added later"))
	encrypted.RoomID = fixture.roomID
	require.NoError(encrypted.Content.ParseRaw(encrypted.Type))
	require.NoError(later.fetchBackupSession(t.Context(), encrypted))
	decrypted, err := machine.DecryptMegolmEvent(t.Context(), encrypted)
	require.NoError(err)
	assert.Equal("added later", decrypted.Content.AsMessage().Body)
	// Known sessions are not fetched again.
	require.NoError(later.fetchBackupSession(t.Context(), encrypted))
	assert.Equal(int64(1), fixture.sessionFetches.Load())
}

func TestFetchBackupSessionRejectsReplacedBackup(t *testing.T) {
	require := require.New(t)
	fixture := newKeyBackupFixture(t)
	mux := http.NewServeMux()
	fixture.handle(t, mux)
	server := httptest.NewServer(mux)
	defer server.Close()
	client, machine := newArchiveMachine(t, server.URL)
	otherKey, err := backup.NewMegolmBackupKey()
	require.NoError(err)
	require.NoError(machine.CryptoStore.PutSecret(t.Context(), cachedBackupKeySecret,
		base64.RawStdEncoding.EncodeToString(otherKey.Bytes())))
	runtime := &Runtime{Client: client, machine: machine}
	encrypted := matrixTestEvent(t, fixture.encryptedEventJSON(t, "$rotated", "unreachable"))
	encrypted.RoomID = fixture.roomID
	require.NoError(encrypted.Content.ParseRaw(encrypted.Type))

	require.ErrorIs(runtime.fetchBackupSession(t.Context(), encrypted), errBackupKeyReplaced)
	require.NoError(runtime.fetchBackupSession(t.Context(), encrypted), "a replaced backup is reported once per sync")
	assert.Zero(t, fixture.sessionFetches.Load())
}

func TestRestoreKeyBackupRequiresRecoverySecret(t *testing.T) {
	err := (&Runtime{}).RestoreKeyBackup(context.Background(), "", false)
	require.ErrorContains(t, err, "recovery secret is required")
}

func TestFetchBackupSessionRequestsEachOutcomeOncePerRun(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := newKeyBackupFixture(t)
	missing := newKeyBackupFixture(t)
	var versionFetches, sessionFetches atomic.Int64
	failVersion := true
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/room_keys/version", func(w http.ResponseWriter, _ *http.Request) {
		versionFetches.Add(1)
		if failVersion {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"errcode":"M_UNKNOWN","error":"unavailable"}`))
			return
		}
		publicKey := base64.RawStdEncoding.EncodeToString(fixture.backupKey.PublicKey().Bytes())
		_, _ = fmt.Fprintf(w, `{"algorithm":"m.megolm_backup.v1.curve25519-aes-sha2","auth_data":{"public_key":%q},"count":0,"etag":"1","version":"1"}`, publicKey)
	})
	mux.HandleFunc("GET /_matrix/client/v3/room_keys/keys/{room}/{session}", func(w http.ResponseWriter, _ *http.Request) {
		sessionFetches.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errcode":"M_NOT_FOUND","error":"No room_keys found"}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, machine := newArchiveMachine(t, server.URL)
	require.NoError(machine.CryptoStore.PutSecret(t.Context(), cachedBackupKeySecret,
		base64.RawStdEncoding.EncodeToString(fixture.backupKey.Bytes())))
	pending := func(f *keyBackupFixture, eventID string) *event.Event {
		evt := matrixTestEvent(t, f.encryptedEventJSON(t, eventID, "body"))
		evt.RoomID = f.roomID
		require.NoError(evt.Content.ParseRaw(evt.Type))
		return evt
	}

	failing := &Runtime{Client: client, machine: machine}
	require.Error(failing.fetchBackupSession(t.Context(), pending(fixture, "$a")))
	for _, eventID := range []string{"$b", "$c"} {
		require.NoError(failing.fetchBackupSession(t.Context(), pending(missing, eventID)))
	}
	assert.Equal(int64(1), versionFetches.Load(), "a failed version request is not repeated in a run")
	assert.Zero(sessionFetches.Load())

	failVersion = false
	healthy := &Runtime{Client: client, machine: machine}
	for _, eventID := range []string{"$d", "$e", "$f"} {
		require.NoError(healthy.fetchBackupSession(t.Context(), pending(missing, eventID)))
	}
	assert.Equal(int64(2), versionFetches.Load())
	assert.Equal(int64(1), sessionFetches.Load(), "events sharing a missing session cost one lookup")
}
