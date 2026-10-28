package matrixtest

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/crypto/backup"
	"maunium.net/go/mautrix/crypto/olm"
	"maunium.net/go/mautrix/crypto/ssss"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func HandleSecretStorage(t *testing.T, mux *http.ServeMux, key *ssss.Key, backupKey *backup.MegolmBackupKey) {
	t.Helper()
	encrypted := key.Encrypt(event.AccountDataMegolmBackupKey.Type, backupKey.Bytes())
	mux.HandleFunc("GET /_matrix/client/v3/user/{user}/account_data/{type}", func(w http.ResponseWriter, r *http.Request) {
		var value any
		switch r.PathValue("type") {
		case "m.secret_storage.default_key":
			value = map[string]string{"key": key.ID}
		case "m.secret_storage.key." + key.ID:
			value = key.Metadata
		case event.AccountDataMegolmBackupKey.Type:
			value = map[string]any{"encrypted": map[string]any{key.ID: encrypted}}
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, err := json.Marshal(value)
		if assert.NoError(t, err) {
			_, _ = w.Write(raw)
		}
	})
}

type BackupCrypto struct {
	Key  *backup.MegolmBackupKey
	Out  olm.OutboundGroupSession
	Data backup.MegolmSessionData
}

func NewBackupCrypto(t *testing.T) *BackupCrypto {
	t.Helper()
	req := require.New(t)
	key, err := backup.NewMegolmBackupKey()
	req.NoError(err)
	out, err := olm.NewOutboundGroupSession()
	req.NoError(err)
	in, err := olm.NewInboundGroupSession([]byte(out.Key()))
	req.NoError(err)
	exported, err := in.Export(0)
	req.NoError(err)
	return &BackupCrypto{Key: key, Out: out, Data: backup.MegolmSessionData{Algorithm: id.AlgorithmMegolmV1, SessionKey: string(exported)}}
}
