package matrix

import (
	"encoding/base64"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/matrix/matrixtest"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/backup"
	"maunium.net/go/mautrix/crypto/olm"
	"maunium.net/go/mautrix/crypto/ssss"
	"maunium.net/go/mautrix/crypto/utils"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type backupFixture struct {
	key             *backup.MegolmBackupKey
	out             olm.OutboundGroupSession
	data            backup.MegolmSessionData
	version         string
	etag            string
	status          int
	roomStatuses    map[string]int
	versionFetches  int
	versionStatus   int
	publicKey       string
	roomFetches     int
	sessionResponse any
}

func newBackupFixture(t *testing.T) *backupFixture {
	t.Helper()
	f := matrixtest.NewBackupCrypto(t)
	return &backupFixture{key: f.Key, out: f.Out, version: "1", etag: "1", data: f.Data}
}

func (f *backupFixture) handle(t *testing.T, mux *http.ServeMux) {
	t.Helper()
	mux.HandleFunc("GET /_matrix/client/v3/room_keys/version", func(w http.ResponseWriter, _ *http.Request) {
		f.versionFetches++
		if f.versionStatus != 0 {
			w.WriteHeader(f.versionStatus)
			return
		}
		publicKey := f.publicKey
		if publicKey == "" {
			publicKey = base64.RawStdEncoding.EncodeToString(f.key.PublicKey().Bytes())
		}
		writeMatrixJSON(t, w, map[string]any{"version": f.version, "etag": f.etag, "algorithm": id.KeyBackupAlgorithmMegolmBackupV1, "auth_data": map[string]any{"public_key": publicKey}})
	})
	mux.HandleFunc("GET /_matrix/client/v3/room_keys/keys/{room}", func(w http.ResponseWriter, r *http.Request) {
		f.roomFetches++
		status := f.status
		if roomStatus := f.roomStatuses[r.PathValue("room")]; roomStatus != 0 {
			status = roomStatus
		}
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"errcode":"M_NOT_FOUND","error":"unavailable"}`))
			return
		}
		if f.sessionResponse != nil {
			writeMatrixJSON(t, w, f.sessionResponse)
			return
		}
		envelope, err := backup.EncryptSessionData(f.key, f.data)
		if !assert.NoError(t, err) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		data := mautrix.RespKeyBackupData[*backup.EncryptedSessionData[backup.MegolmSessionData]]{SessionData: envelope}
		writeMatrixJSON(t, w, map[string]any{"sessions": map[id.SessionID]any{f.out.ID(): data}})
	})
}

func writeMatrixJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if assert.NoError(t, err) {
		_, _ = w.Write(raw)
	}
}

func (f *backupFixture) encrypted(t *testing.T, eventID string, ts int64, typ event.Type, content any) *event.Event {
	t.Helper()
	req := require.New(t)
	raw, err := json.Marshal(content)
	req.NoError(err)
	var fields map[string]any
	req.NoError(json.Unmarshal(raw, &fields))
	var relates *event.RelatesTo
	if relation, exists := fields["m.relates_to"]; exists {
		relationRaw, err := json.Marshal(relation)
		req.NoError(err)
		relates = &event.RelatesTo{}
		req.NoError(json.Unmarshal(relationRaw, relates))
		delete(fields, "m.relates_to")
	}
	plain, err := json.Marshal(map[string]any{"room_id": "!room:example.org", "type": typ.Type, "content": fields})
	req.NoError(err)
	cipher, err := f.out.Encrypt(plain)
	req.NoError(err)
	return &event.Event{ID: id.EventID(eventID), RoomID: "!room:example.org", Sender: "@member:example.org", Timestamp: ts, Type: event.EventEncrypted,
		Content: event.Content{Parsed: &event.EncryptedEventContent{Algorithm: id.AlgorithmMegolmV1, SessionID: f.out.ID(), MegolmCiphertext: cipher, RelatesTo: relates}}}
}

func backupRuntime(t *testing.T, url string, f *backupFixture) *Runtime {
	t.Helper()
	r, err := Open(Credentials{Homeserver: url, UserID: "@archive:example.org", AccessToken: "token", DeviceID: "ARCHIVE", BackupKey: base64.RawStdEncoding.EncodeToString(f.key.Bytes())})
	require.NoError(t, err)
	r.Client.DefaultHTTPRetries = 0
	return r
}

func TestLoadBackupKey(t *testing.T) {
	for _, mode := range []string{"recovery", "passphrase", "unverifiable", "wrong", "corrupt IV", "deleted", "no secret storage", "unknown algorithm"} {
		t.Run(mode, func(t *testing.T) {
			req := require.New(t)
			check := assert.New(t)
			f := newBackupFixture(t)
			key, err := ssss.NewKey("example passphrase")
			req.NoError(err)
			secret := key.RecoveryKey()
			if mode == "passphrase" {
				secret = "example passphrase"
			}
			if mode == "wrong" {
				wrong, err := ssss.NewKey("")
				req.NoError(err)
				secret = wrong.RecoveryKey()
			}
			if mode == "unverifiable" {
				key.Metadata.IV, key.Metadata.MAC = "", ""
			}
			if mode == "corrupt IV" {
				key.Metadata.IV = "invalid"
			}
			if mode == "unknown algorithm" {
				key.Metadata.Algorithm = "example.unknown"
			}
			mux := http.NewServeMux()
			matrixtest.HandleSecretStorage(t, mux, key, f.key)
			if mode == "no secret storage" {
				mux.HandleFunc("GET /_matrix/client/v3/user/{user}/account_data/m.secret_storage.default_key", func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"errcode":"M_NOT_FOUND"}`))
				})
			}
			if mode == "deleted" {
				mux.HandleFunc("GET /_matrix/client/v3/room_keys/version", func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"errcode":"M_NOT_FOUND"}`))
				})
			} else {
				f.handle(t, mux)
			}
			server := httptest.NewServer(mux)
			t.Cleanup(server.Close)
			encoded, err := LoadBackupKey(t.Context(), Credentials{Homeserver: server.URL, UserID: "@archive:example.org", DeviceID: "ARCHIVE", AccessToken: "token"}, secret, mode == "passphrase", utils.DecodeBase58RecoveryKey(secret))
			switch mode {
			case "corrupt IV":
				req.ErrorIs(err, ssss.ErrCorruptedKeyMetadata)
				req.NotErrorIs(err, ssss.ErrIncorrectSSSSKey)
				check.Empty(encoded)
				check.Zero(f.versionFetches)
			case "unknown algorithm":
				req.ErrorContains(err, "unsupported Matrix secret storage algorithm")
				check.Empty(encoded)
				check.Zero(f.versionFetches)
			case "no secret storage":
				req.ErrorContains(err, "--skip-key-backup")
				check.Empty(encoded)
			case "deleted":
				req.ErrorContains(err, "matrix account has no key backup; create one in a Matrix client or use --skip-key-backup")
				req.ErrorIs(err, mautrix.MNotFound)
				check.NotContains(err.Error(), "--recovery-file")
				check.Empty(encoded)
			case "wrong":
				req.ErrorContains(err, "unlock Matrix secret storage")
				req.ErrorIs(err, ssss.ErrIncorrectSSSSKey)
				check.Empty(encoded)
			default:
				req.NoError(err)
				check.Equal(base64.RawStdEncoding.EncodeToString(f.key.Bytes()), encoded)
			}
		})
	}
}

func TestBackupDecrypt(t *testing.T) {
	serve := func(t *testing.T, f *backupFixture) *httptest.Server {
		t.Helper()
		mux := http.NewServeMux()
		f.handle(t, mux)
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		return server
	}
	for _, mode := range []string{"found", "padded key", "invalid key", "corrupt stored key", "missing", "empty envelope", "null plaintext", "early", "transient", "wrong room", "algorithm", "redaction", "replaced"} {
		t.Run(mode, func(t *testing.T) {
			req := require.New(t)
			check := assert.New(t)
			f := newBackupFixture(t)
			switch mode {
			case "padded key":
				f.publicKey = base64.StdEncoding.EncodeToString(f.key.PublicKey().Bytes())
			case "invalid key":
				f.publicKey = "invalid!"
			case "empty envelope", "null plaintext":
				var response any = map[string]any{"session_data": map[string]any{}}
				if mode == "null plaintext" {
					envelope, err := backup.EncryptSessionData[any](f.key, nil)
					req.NoError(err)
					response = map[string]any{"session_data": envelope}
				}
				f.sessionResponse = map[string]any{"sessions": map[id.SessionID]any{f.out.ID(): response}}
			}
			evt := f.encrypted(t, "$message", 1, event.EventMessage, map[string]any{"msgtype": "m.text", "body": "hello"})
			if mode == "redaction" {
				evt = f.encrypted(t, "$redaction", 2, event.EventRedaction, map[string]any{"redacts": "$target"})
			}
			if mode == "missing" {
				evt.Content.AsEncrypted().SessionID = "unknown"
			}
			if mode == "early" {
				in, err := olm.NewInboundGroupSession([]byte(f.out.Key()))
				req.NoError(err)
				exported, err := in.Export(1)
				req.NoError(err)
				f.data.SessionKey = string(exported)
			}
			if mode == "transient" {
				f.status = 503
			}
			if mode == "wrong room" {
				evt.RoomID = "!other:example.org"
			}
			if mode == "algorithm" {
				evt.Content.AsEncrypted().Algorithm = "unknown"
			}
			server := serve(t, f)
			var r *Runtime
			if mode == "corrupt stored key" {
				var err error
				r, err = Open(Credentials{Homeserver: server.URL, UserID: "@archive:example.org", AccessToken: "token", BackupKey: "corrupt!"})
				req.NoError(err)
			} else {
				r = backupRuntime(t, server.URL, f)
			}
			if mode == "replaced" {
				var err error
				f.key, err = backup.NewMegolmBackupKey()
				req.NoError(err)
			}
			plain, _, err := r.decryptWithIndex(t.Context(), evt)
			switch mode {
			case "found", "padded key":
				req.NoError(err)
				req.True(parseContent(plain))
				check.Equal("hello", plain.Content.AsMessage().Body)
				check.True(plain.Mautrix.WasEncrypted)
			case "corrupt stored key":
				req.ErrorContains(r.backupKeyError, "decode Matrix backup key")
				req.EqualError(err, "matrix backup key unavailable")
			case "replaced":
				req.ErrorContains(err, "matrix key backup was replaced")
				check.Contains(err.Error(), "run add-matrix with --recovery-file or --skip-key-backup")
			case "missing", "empty envelope", "null plaintext":
				req.EqualError(err, "matrix session absent or invalid in backup")
				check.Nil(plain)
			case "early":
				req.ErrorContains(err, "decrypt Matrix message")
				req.ErrorIs(err, olm.ErrUnknownMessageIndex)
			case "wrong room":
				req.EqualError(err, "matrix decrypted message belongs to another room")
			case "algorithm":
				req.EqualError(err, "unsupported Matrix encryption algorithm")
			case "redaction":
				req.EqualError(err, "matrix redactions must come from outer server events")
			default:
				req.Error(err)
			}
			if mode == "transient" {
				_, _, err = r.decryptWithIndex(t.Context(), evt)
				req.Error(err)
				check.Equal(2, f.roomFetches)
			}
			if mode == "algorithm" || mode == "replaced" || mode == "invalid key" || mode == "corrupt stored key" {
				check.Zero(f.roomFetches)
			}
			check.Equal(mode == "replaced" || mode == "invalid key", r.failure != nil)
		})
	}
	t.Run("room cache for missing sessions", func(t *testing.T) {
		req := require.New(t)
		check := assert.New(t)
		f := newBackupFixture(t)
		server := serve(t, f)
		r := backupRuntime(t, server.URL, f)
		for i := range 100 {
			evt := f.encrypted(t, strconv.Itoa(i), 1, event.EventMessage, map[string]any{})
			evt.Content.AsEncrypted().SessionID = id.SessionID(strconv.Itoa(i))
			_, _, err := r.decryptWithIndex(t.Context(), evt)
			req.Error(err)
		}
		evt := f.encrypted(t, "$found", 1, event.EventMessage, map[string]any{"msgtype": "m.text", "body": "hello"})
		_, _, err := r.decryptWithIndex(t.Context(), evt)
		req.NoError(err)
		check.Equal(1, f.roomFetches)
		evt.Content.AsEncrypted().SessionID = "absent-after-room-fetch"
		_, _, err = r.decryptWithIndex(t.Context(), evt)
		req.Error(err)
		check.Equal(1, f.roomFetches)
	})
	t.Run("only needed sessions", func(t *testing.T) {
		req := require.New(t)
		check := assert.New(t)
		f := newBackupFixture(t)
		unused := newBackupFixture(t)
		wantedData, err := backup.EncryptSessionData(f.key, f.data)
		req.NoError(err)
		unusedData, err := backup.EncryptSessionData(f.key, unused.data)
		req.NoError(err)
		f.sessionResponse = map[string]any{"sessions": map[id.SessionID]any{f.out.ID(): map[string]any{"session_data": wantedData}, unused.out.ID(): map[string]any{"session_data": unusedData}}}
		server := serve(t, f)
		r := backupRuntime(t, server.URL, f)
		evt := f.encrypted(t, "$wanted", 1, event.EventMessage, map[string]any{"msgtype": "m.text", "body": "wanted"})
		plain, _, err := r.decryptWithIndex(t.Context(), evt)
		req.NoError(err)
		req.True(parseContent(plain))
		check.Equal("wanted", plain.Content.AsMessage().Body)
		unusedKey := backupSessionKey{evt.RoomID, unused.out.ID()}
		check.Equal(unusedData.Ciphertext, r.sessionData[unusedKey].SessionData.Ciphertext)
		check.NotContains(r.sessions, unusedKey)
	})
	t.Run("relations prefer outer event", func(t *testing.T) {
		for _, kind := range []string{"reply", "reaction", "edit"} {
			for _, outer := range []bool{false, true} {
				t.Run(kind+"/outer="+strconv.FormatBool(outer), func(t *testing.T) {
					req := require.New(t)
					f := newBackupFixture(t)
					typ := event.EventMessage
					inner := map[string]any{"m.in_reply_to": map[string]any{"event_id": "$inner"}}
					outerRelation := &event.RelatesTo{InReplyTo: &event.InReplyTo{EventID: "$outer"}}
					if kind != "reply" {
						relType := event.RelReplace
						if kind == "reaction" {
							typ, relType = event.EventReaction, event.RelAnnotation
						}
						inner = map[string]any{"rel_type": relType, "event_id": "$inner", "key": "yes"}
						outerRelation = &event.RelatesTo{Type: relType, EventID: "$outer", Key: "yes"}
					}
					payload, err := json.Marshal(map[string]any{"room_id": "!room:example.org", "type": typ.Type, "content": map[string]any{"msgtype": "m.text", "body": "hello", "m.relates_to": inner}})
					req.NoError(err)
					cipher, err := f.out.Encrypt(payload)
					req.NoError(err)
					evt := &event.Event{ID: "$message", RoomID: "!room:example.org", Type: event.EventEncrypted, Content: event.Content{Parsed: &event.EncryptedEventContent{Algorithm: id.AlgorithmMegolmV1, SessionID: f.out.ID(), MegolmCiphertext: cipher}}}
					if outer {
						evt.Content.AsEncrypted().RelatesTo = outerRelation
					}
					server := serve(t, f)
					plain, _, err := backupRuntime(t, server.URL, f).decryptWithIndex(t.Context(), evt)
					req.NoError(err)
					req.True(parseContent(plain))
					want := id.EventID("")
					if outer {
						want = "$outer"
					}
					var got id.EventID
					switch kind {
					case "reply":
						got = plain.Content.AsMessage().RelatesTo.GetReplyTo()
					case "reaction":
						got = plain.Content.AsReaction().RelatesTo.EventID
					default:
						got = plain.Content.AsMessage().RelatesTo.GetReplaceID()
					}
					assert.Equal(t, want, got)
				})
			}
		}
	})
}

func TestBackupStampSeparatesVersionAndETag(t *testing.T) {
	f := newBackupFixture(t)
	mux := http.NewServeMux()
	f.handle(t, mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	f.version, f.etag = "a:b", "c"
	first := backupRuntime(t, server.URL, f)
	require.NoError(t, first.prepareBackup(t.Context()))
	f.version, f.etag = "a", "b:c"
	second := backupRuntime(t, server.URL, f)
	require.NoError(t, second.prepareBackup(t.Context()))
	assert.NotEqual(t, first.stamp, second.stamp)
}
