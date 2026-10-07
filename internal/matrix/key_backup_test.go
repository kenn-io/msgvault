//go:build goolm

package matrix

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/crypto/backup"
	"maunium.net/go/mautrix/crypto/ssss"
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

// shareFromIndex gives machine a copy of the room key that starts at index, as
// a key forwarded after the first messages were sent would.
func (f *keyBackupFixture) shareFromIndex(t *testing.T, machine *crypto.OlmMachine, index uint32) {
	t.Helper()
	inbound, err := crypto.NewInboundGroupSession(f.session.SenderKey, f.session.SenderClaimedKeys.Ed25519,
		f.roomID, f.outboundKey, 0, 0, f.session.SharedHistory, false)
	require.NoError(t, err)
	exported, err := inbound.Internal.Export(index)
	require.NoError(t, err)
	session := f.session
	session.SessionKey = string(exported)
	later, err := machine.ImportRoomKeyFromBackupWithoutSaving(t.Context(), "", f.roomID, nil, f.sessionID, &session)
	require.NoError(t, err)
	require.Equal(t, index, later.Internal.FirstKnownIndex())
	require.NoError(t, machine.CryptoStore.PutGroupSession(t.Context(), later))
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

func TestSyncFetchesEarlierBackupKeyWhenStoredSessionStartsLater(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := newKeyBackupFixture(t)
	early := fixture.encryptedEventJSON(t, "$early", "before the forwarded key")
	late := fixture.encryptedEventJSON(t, "$late", "after the forwarded key")
	mux := http.NewServeMux()
	fixture.handle(t, mux)
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"next_batch":"next-1","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"events":[%s,%s]}}}}}`, early, late)
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
	require.NoError(machine.CryptoStore.PutSecret(t.Context(), cachedBackupKeySecret,
		base64.RawStdEncoding.EncodeToString(fixture.backupKey.Bytes())))
	// The local copy starts at index 1 and so decrypts only "$late"; the backup
	// holds the same session from index 0.
	fixture.shareFromIndex(t, machine, 1)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	runtime := &Runtime{Client: client, machine: machine, decryptEvent: machine.DecryptMegolmEvent}
	summary, err := NewImporter(st, runtime).Import(t.Context(), ImportOptions{UserID: source.Identifier})
	require.NoError(err)

	assert.Equal(int64(1), summary.UndecryptableRecovered)
	assert.Equal(int64(1), fixture.sessionFetches.Load())
	messages, err := st.MessageExistsBatch(source.ID, []string{"$early", "$late"})
	require.NoError(err)
	for eventID, want := range map[string]string{"$early": "before the forwarded key", "$late": "after the forwarded key"} {
		body, err := st.GetMessageBodyText(messages[eventID])
		require.NoError(err)
		assert.Equal(want, body, eventID)
	}
	stored, err := machine.CryptoStore.GetGroupSession(t.Context(), fixture.roomID, fixture.sessionID)
	require.NoError(err)
	assert.Equal(uint32(0), stored.Internal.FirstKnownIndex())
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

// TestRestoreKeyBackupUnlocksSecretStorage stores the backup key in secret
// storage the way Element does, then restores the backup from the recovery key
// or the passphrase that unlocks it.
func TestRestoreKeyBackupUnlocksSecretStorage(t *testing.T) {
	for _, tc := range []struct {
		name       string
		passphrase string
	}{
		{name: "recovery key"},
		{name: "passphrase", passphrase: "correct horse battery staple"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			fixture := newKeyBackupFixture(t)
			accountData := map[string][]byte{}
			var accountDataMu sync.Mutex
			mux := http.NewServeMux()
			fixture.handle(t, mux)
			mux.HandleFunc("GET /_matrix/client/v3/room_keys/keys", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("version") != "1" {
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"errcode":"M_NOT_FOUND","error":"Unknown backup version"}`))
					return
				}
				encrypted, err := backup.EncryptSessionData(fixture.backupKey, fixture.session)
				if !assert.NoError(err) {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				body, err := json.Marshal(mautrix.RespRoomKeys[*backup.EncryptedSessionData[backup.MegolmSessionData]]{
					Rooms: map[id.RoomID]mautrix.RespRoomKeyBackup[*backup.EncryptedSessionData[backup.MegolmSessionData]]{
						fixture.roomID: {Sessions: map[id.SessionID]mautrix.RespKeyBackupData[*backup.EncryptedSessionData[backup.MegolmSessionData]]{
							fixture.sessionID: {SessionData: encrypted},
						}},
					},
				})
				if !assert.NoError(err) {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				_, _ = w.Write(body)
			})
			mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/{type}", func(w http.ResponseWriter, r *http.Request) {
				accountDataMu.Lock()
				content, ok := accountData[r.PathValue("type")]
				accountDataMu.Unlock()
				if !ok {
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"errcode":"M_NOT_FOUND","error":"Account data not found"}`))
					return
				}
				_, _ = w.Write(content)
			})
			mux.HandleFunc("PUT /_matrix/client/v3/user/@archive:example.org/account_data/{type}", func(w http.ResponseWriter, r *http.Request) {
				var content bytes.Buffer
				if _, err := content.ReadFrom(r.Body); !assert.NoError(err) {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				accountDataMu.Lock()
				accountData[r.PathValue("type")] = content.Bytes()
				accountDataMu.Unlock()
				_, _ = w.Write([]byte(`{}`))
			})
			server := httptest.NewServer(mux)
			defer server.Close()
			client, machine := newArchiveMachine(t, server.URL)

			// Another client of the account sets up secret storage and stores
			// the backup key in it, encrypted with the secret-storage key.
			secretStorage := ssss.NewSSSSMachine(client)
			key, err := secretStorage.GenerateAndUploadKey(t.Context(), tc.passphrase)
			require.NoError(err)
			require.NoError(secretStorage.SetDefaultKeyID(t.Context(), key.ID))
			require.NoError(secretStorage.SetEncryptedAccountData(t.Context(), event.AccountDataMegolmBackupKey,
				fixture.backupKey.Bytes(), key))
			secret := key.RecoveryKey()
			if tc.passphrase != "" {
				secret = tc.passphrase
			}

			encrypted := matrixTestEvent(t, fixture.encryptedEventJSON(t, "$backed-up", "from backup"))
			encrypted.RoomID = fixture.roomID
			require.NoError(encrypted.Content.ParseRaw(encrypted.Type))
			_, err = machine.DecryptMegolmEvent(t.Context(), encrypted)
			require.Error(err, "the room key is only in the backup")

			runtime := &Runtime{Client: client, machine: machine}
			require.NoError(runtime.RestoreKeyBackup(t.Context(), secret, tc.passphrase != ""))

			decrypted, err := machine.DecryptMegolmEvent(t.Context(), encrypted)
			require.NoError(err)
			assert.Equal("from backup", decrypted.Content.AsMessage().Body)
			cached, err := machine.CryptoStore.GetSecret(t.Context(), cachedBackupKeySecret)
			require.NoError(err)
			assert.Equal(base64.RawStdEncoding.EncodeToString(fixture.backupKey.Bytes()), cached)
		})
	}
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

// encryptedEditJSON encrypts an edit of target with the fixture's session.
func (f *keyBackupFixture) encryptedEditJSON(t *testing.T, eventID string, target id.EventID, body string) string {
	t.Helper()
	content, err := f.sender.EncryptMegolmEvent(t.Context(), f.roomID, event.EventMessage, &event.MessageEventContent{
		MsgType: event.MsgText, Body: "* " + body,
		NewContent: &event.MessageEventContent{MsgType: event.MsgText, Body: body},
		RelatesTo:  &event.RelatesTo{Type: event.RelReplace, EventID: target},
	})
	require.NoError(t, err)
	encoded, err := json.Marshal(content)
	require.NoError(t, err)
	return fmt.Sprintf(`{"type":"m.room.encrypted","event_id":%q,"sender":"@sender:example.org","origin_server_ts":2000,"content":%s}`, eventID, encoded)
}

// relatedEditFixture archives "$original" in st as a pending placeholder whose
// key the archive receives later, while its only edit is listed by /relations
// and encrypted with a session that only the server-side backup holds. Each
// /sync returns an empty timeline until the returned function sets one.
func relatedEditFixture(t *testing.T, st *store.Store) (*keyBackupFixture, *Runtime, *store.Source, func(timeline string)) {
	t.Helper()
	require := require.New(t)
	fixture := newKeyBackupFixture(t)
	edit := fixture.encryptedEditJSON(t, "$edit", "$original", "edited")
	var timeline atomic.Pointer[string]
	mux := http.NewServeMux()
	fixture.handle(t, mux)
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
		if events := timeline.Load(); events != nil {
			_, _ = fmt.Fprintf(w, `{"next_batch":"replayed","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"events":[%s]}}}}}`, *events)
			return
		}
		_, _ = w.Write([]byte(`{"next_batch":"next","rooms":{"join":{}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/joined_members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"joined":{"@archive:example.org":{"display_name":"Archive"},"@sender:example.org":{"display_name":"Sender"}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v1/rooms/{room}/relations/{event}/m.replace", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("event") != "$original" {
			_, _ = w.Write([]byte(`{"chunk":[]}`))
			return
		}
		_, _ = fmt.Fprintf(w, `{"chunk":[%s]}`, edit)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client, machine := newArchiveMachine(t, server.URL)
	original := matrixTestEvent(t, `{"type":"m.room.encrypted","event_id":"$original","sender":"@sender:example.org","origin_server_ts":1000,"content":{"algorithm":"m.megolm.v1.aes-sha2","ciphertext":"opaque-original","session_id":"other-session","sender_key":"key"}}`)
	originalKnown := false
	runtime := &Runtime{Client: client, machine: machine, decryptEvent: func(ctx context.Context, evt *event.Event) (*event.Event, error) {
		if evt.ID != "$original" {
			return machine.DecryptMegolmEvent(ctx, evt)
		}
		if !originalKnown {
			return nil, errors.New("synthetic missing session")
		}
		return matrixTestEvent(t, `{"type":"m.room.message","event_id":"$original","sender":"@sender:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"original"}}`), nil
	}}
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, fixture.roomID.String(), "Example room")
	require.NoError(err)
	require.NoError(NewImporter(st, runtime).persistEvent(t.Context(), source.ID, conversationID, original,
		attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
	originalKnown = true
	return fixture, runtime, source, func(events string) { timeline.Store(&events) }
}

func TestRecoveredOriginalFetchesBackupKeyForRelatedEdit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	// CopySubset reads an SQLite archive by path.
	dbPath := filepath.Join(t.TempDir(), "msgvault.db")
	st, err := store.Open(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	fixture, runtime, source, replay := relatedEditFixture(t, st)
	require.NoError(runtime.machine.CryptoStore.PutSecret(t.Context(), cachedBackupKeySecret,
		base64.RawStdEncoding.EncodeToString(fixture.backupKey.Bytes())))

	_, err = NewImporter(st, runtime).Import(t.Context(), ImportOptions{UserID: source.Identifier})
	require.NoError(err)

	assert.Equal("edited", archivedBody(t, st, source.ID, "$original"))
	assert.Equal(int64(1), fixture.sessionFetches.Load())
	messages, err := st.MessageExistsBatch(source.ID, []string{"$edit"})
	require.NoError(err)
	assert.Zero(messages["$edit"], "a decrypted related edit leaves no placeholder")
	ciphertext, _, err := st.MatrixEncryptedEvent(source.ID, "$edit")
	require.NoError(err, "the applied edit's ciphertext is retained")
	var retained event.Event
	require.NoError(json.Unmarshal(ciphertext, &retained))
	assert.Equal(event.EventEncrypted, retained.Type)

	// A full replay on a device without the edit's key sees the edit in the
	// timeline and must recognise it as decrypted before.
	replay(fixture.encryptedEditJSON(t, "$edit", "$original", "edited"))
	keyless := &Runtime{Client: runtime.Client}
	sum, err := NewImporter(st, keyless).Import(t.Context(), ImportOptions{UserID: source.Identifier, Full: true})
	require.NoError(err)
	assert.Zero(sum.Undecryptable)
	messages, err = st.MessageExistsBatch(source.ID, []string{"$edit"})
	require.NoError(err)
	assert.Zero(messages["$edit"], "a keyless replay adds no placeholder for the applied edit")
	_, pending := pendingUndecryptable(t, st, source.ID, "$edit")
	assert.False(pending)
	assert.Equal("edited", archivedBody(t, st, source.ID, "$original"))

	dstDir := filepath.Join(t.TempDir(), "subset")
	_, err = store.CopySubset(dbPath, dstDir, 1, false)
	require.NoError(err)
	subset, err := store.Open(filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = subset.Close() })
	copied, _, err := subset.MatrixEncryptedEvent(source.ID, "$edit")
	require.NoError(err, "the subset keeps the applied edit's ciphertext")
	assert.Equal(ciphertext, copied)
}

func TestUndecryptableRelatedEditIsRetriedByLaterSync(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	fixture, runtime, source, _ := relatedEditFixture(t, st)

	// No backup key is cached yet, so the edit cannot be decrypted.
	_, err := NewImporter(st, runtime).Import(t.Context(), ImportOptions{UserID: source.Identifier})
	require.NoError(err)
	assert.Equal("original", archivedBody(t, st, source.ID, "$original"))
	_, pending, err := st.MatrixUndecryptableEvent(source.ID, "$edit")
	require.NoError(err)
	require.True(pending, "the related edit keeps retry state")

	require.NoError(runtime.machine.CryptoStore.PutSecret(t.Context(), cachedBackupKeySecret,
		base64.RawStdEncoding.EncodeToString(fixture.backupKey.Bytes())))
	// Each sync opens a new runtime, which reads the cached backup key again.
	next := &Runtime{Client: runtime.Client, machine: runtime.machine, decryptEvent: runtime.decryptEvent}
	_, err = NewImporter(st, next).Import(t.Context(), ImportOptions{UserID: source.Identifier})
	require.NoError(err)
	assert.Equal("edited", archivedBody(t, st, source.ID, "$original"))
	_, pending, err = st.MatrixUndecryptableEvent(source.ID, "$edit")
	require.NoError(err)
	assert.False(pending)
}
