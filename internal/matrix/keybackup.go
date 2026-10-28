package matrix

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/backup"
	"maunium.net/go/mautrix/crypto/goolm"
	"maunium.net/go/mautrix/crypto/olm"
	"maunium.net/go/mautrix/crypto/ssss"
	"maunium.net/go/mautrix/crypto/utils"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func init() { goolm.Register() }

type backupSessionKey struct {
	room    id.RoomID
	session id.SessionID
}

type backupState struct {
	skipBackup  bool
	key         *backup.MegolmBackupKey
	version     id.KeyBackupVersion
	stamp       string
	prepared    bool
	failure     error
	sessions    map[backupSessionKey]olm.InboundGroupSession
	sessionData map[backupSessionKey]mautrix.RespKeyBackupData[backup.EncryptedSessionData[backup.MegolmSessionData]]
	rooms       map[id.RoomID]bool
}

func (r *Runtime) setBackupKey(encoded string) error {
	if encoded == "" {
		return nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("decode Matrix backup key: %w", err)
	}
	r.key, err = backup.MegolmBackupKeyFromBytes(raw)
	if err != nil {
		return fmt.Errorf("decode Matrix backup key: %w", err)
	}
	return nil
}

// LoadBackupKey unlocks the account's backup key without retaining the recovery secret.
func LoadBackupKey(ctx context.Context, creds Credentials, secret string, passphrase bool, recoveryKey []byte) (string, error) {
	creds.BackupKey = ""
	r, err := Open(creds)
	if err != nil {
		return "", err
	}
	mach := ssss.NewSSSSMachine(r.Client)
	keyID, metadata, err := mach.GetDefaultKeyData(ctx)
	if err != nil {
		if errors.Is(err, ssss.ErrNoDefaultKeyID) || errors.Is(err, mautrix.MNotFound) {
			return "", fmt.Errorf("matrix account has no secret storage; use --skip-key-backup to proceed: %w", err)
		}
		return "", fmt.Errorf("load Matrix secret storage: %w", err)
	}
	if metadata.Algorithm != ssss.AlgorithmAESHMACSHA2 {
		return "", fmt.Errorf("unsupported Matrix secret storage algorithm %q", metadata.Algorithm)
	}
	var key *ssss.Key
	if passphrase {
		key, err = metadata.VerifyPassphrase(keyID, secret)
	} else {
		if len(recoveryKey) != 32 {
			return "", fmt.Errorf("unlock Matrix secret storage: %w", ssss.ErrInvalidRecoveryKey)
		}
		key, err = metadata.VerifyRecoveryKey(keyID, utils.EncodeBase58RecoveryKey(recoveryKey))
	}
	if err != nil && !errors.Is(err, ssss.ErrUnverifiableKey) {
		return "", fmt.Errorf("unlock Matrix secret storage: %w", err)
	}
	raw, err := mach.GetDecryptedAccountData(ctx, event.AccountDataMegolmBackupKey, key)
	if err != nil {
		return "", fmt.Errorf("decrypt Matrix backup secret: %w", err)
	}
	r.key, err = backup.MegolmBackupKeyFromBytes(raw)
	if err != nil {
		return "", fmt.Errorf("decode Matrix backup secret: %w", err)
	}
	if err := r.prepareBackup(ctx); err != nil {
		if errors.Is(err, mautrix.MNotFound) {
			return "", fmt.Errorf("matrix account has no key backup; create one in a Matrix client or use --skip-key-backup: %w", errors.Unwrap(err))
		}
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(raw), nil
}

func (r *Runtime) prepareBackup(ctx context.Context) error {
	if r.prepared {
		return r.failure
	}
	r.prepared = true
	r.sessions = map[backupSessionKey]olm.InboundGroupSession{}
	r.rooms = map[id.RoomID]bool{}
	r.sessionData = map[backupSessionKey]mautrix.RespKeyBackupData[backup.EncryptedSessionData[backup.MegolmSessionData]]{}
	info, err := r.Client.GetKeyBackupLatestVersion(ctx)
	if err != nil {
		if errors.Is(err, mautrix.MNotFound) {
			r.failure = fmt.Errorf("matrix account has no key backup; run add-matrix with --recovery-file or --skip-key-backup: %w", err)
		} else {
			r.failure = fmt.Errorf("load Matrix key backup: %w", err)
		}
	} else if publicKey, decodeErr := base64.RawStdEncoding.DecodeString(strings.TrimRight(string(info.AuthData.PublicKey), "=")); decodeErr != nil || info.Algorithm != id.KeyBackupAlgorithmMegolmBackupV1 || !bytes.Equal(publicKey, r.key.PublicKey().Bytes()) {
		r.failure = errors.New("matrix key backup was replaced; run add-matrix with --recovery-file or --skip-key-backup")
	} else {
		r.version, r.stamp = info.Version, fmt.Sprintf("%d:%s%s", len(info.Version), info.Version, info.ETag)
	}
	return r.failure
}

func (r *Runtime) cacheSession(key backupSessionKey, data mautrix.RespKeyBackupData[backup.EncryptedSessionData[backup.MegolmSessionData]]) {
	r.sessions[key] = nil
	if data.SessionData.Ephemeral.PublicKey == nil {
		return
	}
	plain, err := data.SessionData.Decrypt(r.key)
	if err != nil || plain.Algorithm != id.AlgorithmMegolmV1 {
		return
	}
	sess, err := olm.InboundGroupSessionImport([]byte(plain.SessionKey))
	if err == nil && sess.ID() == key.session {
		r.sessions[key] = sess
	}
}

var errRoomBackupFetch = errors.New("fetch Matrix room backup")

func (r *Runtime) fetchRoomBackup(ctx context.Context, room id.RoomID) error {
	if r.rooms[room] {
		return nil
	}
	data, err := r.Client.GetKeyBackupForRoom(ctx, r.version, room)
	if err != nil && !backupSessionMissing(err) {
		failure := fmt.Errorf("%w: %w", errRoomBackupFetch, err)
		var response mautrix.HTTPError
		if errors.Is(err, mautrix.MUnknownToken) || errors.Is(err, mautrix.MMissingToken) || errors.Is(err, mautrix.MForbidden) || (errors.As(err, &response) && response.Response != nil && (response.Response.StatusCode == http.StatusUnauthorized || response.Response.StatusCode == http.StatusForbidden)) {
			r.failure = failure
		}
		return failure
	}
	r.rooms[room] = true
	if data != nil {
		for session, value := range data.Sessions {
			r.sessionData[backupSessionKey{room, session}] = value
		}
	}
	return nil
}

func (r *Runtime) decrypt(ctx context.Context, evt *event.Event) (*event.Event, error) {
	plain, _, err := r.decryptWithIndex(ctx, evt)
	return plain, err
}

func (r *Runtime) decryptWithIndex(ctx context.Context, evt *event.Event) (*event.Event, uint, error) {
	if r.skipBackup || r.key == nil {
		return nil, 0, errors.New("matrix backup key unavailable")
	}
	if err := r.prepareBackup(ctx); err != nil {
		return nil, 0, err
	}
	if !parseContent(evt) {
		return nil, 0, errors.New("invalid encrypted Matrix content")
	}
	content := evt.Content.AsEncrypted()
	if content.Algorithm != id.AlgorithmMegolmV1 {
		return nil, 0, errors.New("unsupported Matrix encryption algorithm")
	}
	key := backupSessionKey{evt.RoomID, content.SessionID}
	if err := r.fetchRoomBackup(ctx, key.room); err != nil {
		return nil, 0, err
	}
	if _, cached := r.sessions[key]; !cached {
		r.cacheSession(key, r.sessionData[key])
		delete(r.sessionData, key)
	}
	sess := r.sessions[key]
	if sess == nil {
		return nil, 0, errors.New("matrix session absent or invalid in backup")
	}
	plain, index, err := sess.Decrypt(content.MegolmCiphertext)
	if err != nil {
		return nil, 0, fmt.Errorf("decrypt Matrix message: %w", err)
	}
	var payload struct {
		RoomID  id.RoomID     `json:"room_id"`
		Type    event.Type    `json:"type"`
		Content event.Content `json:"content"`
	}
	if err := json.Unmarshal(plain, &payload); err != nil {
		return nil, 0, err
	}
	if payload.RoomID != evt.RoomID {
		return nil, 0, errors.New("matrix decrypted message belongs to another room")
	}
	if payload.Type.Type == event.EventRedaction.Type {
		return nil, 0, errors.New("matrix redactions must come from outer server events")
	}
	// MSC4392-style senders lose reply/reaction links by design because inner relations are ignored.
	delete(payload.Content.Raw, "m.relates_to")
	if content.RelatesTo != nil {
		if payload.Content.Raw == nil {
			payload.Content.Raw = map[string]any{}
		}
		payload.Content.Raw["m.relates_to"] = content.RelatesTo
	}
	payload.Content.VeryRaw, err = json.Marshal(payload.Content.Raw)
	if err != nil {
		return nil, 0, err
	}
	result := *evt
	payload.Type.Class = evt.Type.Class
	result.Type, result.Content = payload.Type, payload.Content
	result.Mautrix.WasEncrypted = true
	return &result, index, nil
}

func backupSessionMissing(err error) bool {
	var response mautrix.HTTPError
	return errors.As(err, &response) && response.RespError != nil && response.Response != nil && response.Response.StatusCode == http.StatusNotFound && errors.Is(err, mautrix.MNotFound)
}

func (r *Runtime) releaseRoom(room id.RoomID) {
	for key := range r.sessions {
		if key.room == room {
			delete(r.sessions, key)
		}
	}
	for key := range r.sessionData {
		if key.room == room {
			delete(r.sessionData, key)
		}
	}
	delete(r.rooms, room)
}
