// Package matrix implements the native, read-only Matrix archive source.
package matrix

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/fileutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/crypto/backup"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/crypto/ssss"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

const DeviceDisplayName = "msgvault (read-only)"

func validateHomeserverURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("invalid Matrix homeserver URL %q", raw)
	}
	if u.Scheme == "https" {
		return nil
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if u.Scheme == "http" && (host == "localhost" || net.ParseIP(host).IsLoopback()) {
		return nil
	}
	return fmt.Errorf("matrix homeserver URL must use HTTPS unless its host is loopback: %q", raw)
}

// Runtime owns a mautrix client and its persistent pure-Go Olm machine.
type Runtime struct {
	Client *mautrix.Client
	Crypto *cryptohelper.CryptoHelper
	// decryptEvent is a test seam for exercising importer behavior with a real
	// OlmMachine without constructing a network-backed CryptoHelper.
	decryptEvent func(context.Context, *event.Event) (*event.Event, error)
	// machine is the matching test seam for key-backup access.
	machine *crypto.OlmMachine

	backupLoaded  bool
	backupKey     *backup.MegolmBackupKey
	backupVersion id.KeyBackupVersion
	// backupSessions records each (room, session) lookup attempted this run,
	// so many pending events sharing a session cost one request.
	backupSessions map[backupSessionKey]struct{}
}

type backupSessionKey struct {
	roomID    id.RoomID
	sessionID id.SessionID
}

// cachedBackupKeySecret names the Megolm backup decryption key in the local
// crypto store. The store encrypts secrets with the account's pickle key.
const cachedBackupKeySecret id.Secret = "net.msgvault.megolm_backup.v1" // #nosec G101 -- secret name, not a credential value

func (r *Runtime) olmMachine() *crypto.OlmMachine {
	if r == nil {
		return nil
	}
	if r.machine != nil {
		return r.machine
	}
	if r.Crypto != nil {
		return r.Crypto.Machine()
	}
	return nil
}

func (r *Runtime) canDecrypt() bool {
	return r != nil && (r.Crypto != nil || r.decryptEvent != nil)
}

func (r *Runtime) decrypt(ctx context.Context, evt *event.Event) (*event.Event, error) {
	if r == nil {
		return nil, errors.New("matrix runtime is unavailable")
	}
	if r.decryptEvent != nil {
		decrypted, err := r.decryptEvent(ctx, evt)
		if err != nil {
			return nil, fmt.Errorf("decrypt Matrix event: %w", err)
		}
		return decrypted, nil
	}
	if r.Crypto == nil {
		return nil, errors.New("matrix crypto is unavailable")
	}
	decrypted, err := r.Crypto.Decrypt(ctx, evt)
	if err != nil {
		return nil, fmt.Errorf("decrypt Matrix event: %w", err)
	}
	return decrypted, nil
}

// Login creates a dedicated Matrix device using a password or m.login.token.
func Login(ctx context.Context, homeserver, userID, secret string, tokenLogin bool) (Credentials, error) {
	if err := validateHomeserverURL(homeserver); err != nil {
		return Credentials{}, err
	}
	cli, err := mautrix.NewClient(homeserver, id.UserID(userID), "")
	if err != nil {
		return Credentials{}, fmt.Errorf("create Matrix client: %w", err)
	}
	req := &mautrix.ReqLogin{
		Type:                     mautrix.AuthTypePassword,
		Identifier:               mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: userID},
		Password:                 secret,
		InitialDeviceDisplayName: DeviceDisplayName,
		StoreCredentials:         true,
	}
	var resp *mautrix.RespLogin
	if tokenLogin {
		// ReqLogin always serializes Identifier, but m.login.token does not use
		// the password-only identifier fields.
		resp = &mautrix.RespLogin{}
		_, err = cli.MakeFullRequest(ctx, mautrix.FullRequest{
			Method: http.MethodPost,
			URL:    cli.BuildClientURL("v3", "login"),
			RequestJSON: struct {
				Type                     mautrix.AuthType `json:"type"`
				Token                    string           `json:"token"`
				InitialDeviceDisplayName string           `json:"initial_device_display_name"`
			}{
				Type:                     mautrix.AuthTypeToken,
				Token:                    secret,
				InitialDeviceDisplayName: DeviceDisplayName,
			},
			ResponseJSON:     resp,
			SensitiveContent: true,
		})
	} else {
		resp, err = cli.Login(ctx, req)
	}
	if err != nil {
		return Credentials{}, fmt.Errorf("matrix login: %w", err)
	}
	if resp.UserID == "" || resp.DeviceID == "" || resp.AccessToken == "" {
		return Credentials{}, errors.New("matrix login response is missing user, device, or access token")
	}
	if resp.UserID != id.UserID(userID) {
		mismatchErr := fmt.Errorf("matrix login returned user %s, expected %s", resp.UserID, userID)
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		cleanupErr := Logout(cleanupCtx, Credentials{
			Homeserver: homeserver, UserID: resp.UserID.String(), DeviceID: resp.DeviceID.String(), AccessToken: resp.AccessToken,
		})
		return Credentials{}, errors.Join(mismatchErr, cleanupErr)
	}
	pickleKey, err := generatePickleKey()
	if err != nil {
		return Credentials{}, err
	}
	return Credentials{
		Homeserver: homeserver, UserID: resp.UserID.String(), DeviceID: resp.DeviceID.String(),
		AccessToken: resp.AccessToken, PickleKey: pickleKey,
	}, nil
}

func generatePickleKey() (string, error) {
	pickle := make([]byte, 32)
	if _, err := rand.Read(pickle); err != nil {
		return "", fmt.Errorf("generate Matrix crypto key: %w", err)
	}
	return base64.RawStdEncoding.EncodeToString(pickle), nil
}

// Open initializes the persistent crypto and state stores for an account.
func Open(ctx context.Context, creds Credentials, cryptoPath string) (*Runtime, error) {
	if err := validateHomeserverURL(creds.Homeserver); err != nil {
		return nil, err
	}
	pickle, err := base64.RawStdEncoding.DecodeString(creds.PickleKey)
	if err != nil {
		return nil, fmt.Errorf("decode Matrix crypto key: %w", err)
	}
	if len(pickle) == 0 {
		return nil, errors.New("decode Matrix crypto key: key is empty")
	}
	if err := fileutil.SecureMkdirAll(filepath.Dir(cryptoPath), 0o700); err != nil {
		return nil, fmt.Errorf("create Matrix crypto directory: %w", err)
	}
	cli, err := mautrix.NewClient(creds.Homeserver, id.UserID(creds.UserID), creds.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("create Matrix client: %w", err)
	}
	cli.DeviceID = id.DeviceID(creds.DeviceID)
	helper, err := cryptohelper.NewCryptoHelper(cli, pickle, cryptoPath)
	if err != nil {
		return nil, fmt.Errorf("create Matrix crypto helper: %w", err)
	}
	if err := helper.Init(ctx); err != nil {
		_ = helper.Close()
		return nil, fmt.Errorf("initialize Matrix crypto: %w", err)
	}
	helper.Machine().AllowKeyShare = func(context.Context, *id.Device, event.RequestedKeyInfo) *crypto.KeyShareRejection {
		return &crypto.KeyShareRejectNoResponse
	}
	if err := os.Chmod(cryptoPath, 0o600); err != nil && !os.IsNotExist(err) {
		_ = helper.Close()
		return nil, fmt.Errorf("secure Matrix crypto store: %w", err)
	}
	return &Runtime{Client: cli, Crypto: helper}, nil
}

// Logout deletes the dedicated Matrix device represented by creds.
func Logout(ctx context.Context, creds Credentials) error {
	if err := validateHomeserverURL(creds.Homeserver); err != nil {
		return err
	}
	cli, err := mautrix.NewClient(creds.Homeserver, id.UserID(creds.UserID), creds.AccessToken)
	if err != nil {
		return fmt.Errorf("create Matrix logout client: %w", err)
	}
	cli.DeviceID = id.DeviceID(creds.DeviceID)
	if _, err := cli.Logout(ctx); err != nil {
		return fmt.Errorf("logout Matrix device %s: %w", creds.DeviceID, err)
	}
	return nil
}

// IsUnknownToken reports whether the homeserver says the credential has
// already been revoked or expired.
func IsUnknownToken(err error) bool {
	return errors.Is(err, mautrix.MUnknownToken)
}

func (r *Runtime) Close() error {
	if r == nil || r.Crypto == nil {
		return nil
	}
	if err := r.Crypto.Close(); err != nil {
		return fmt.Errorf("close Matrix crypto helper: %w", err)
	}
	return nil
}

// RestoreKeyBackup derives the SSSS key from the one-time recovery secret,
// decrypts the backup key, and imports the server-side room-key backup.
func (r *Runtime) RestoreKeyBackup(ctx context.Context, secret string, passphrase bool) error {
	if secret == "" {
		return errors.New("matrix recovery secret is required to restore key backup")
	}
	mach := ssss.NewSSSSMachine(r.Client)
	keyID, metadata, err := mach.GetDefaultKeyData(ctx)
	if err != nil {
		return fmt.Errorf("load Matrix secret-storage metadata: %w", err)
	}
	var key *ssss.Key
	if passphrase {
		key, err = metadata.VerifyPassphrase(keyID, secret)
	} else {
		key, err = metadata.VerifyRecoveryKey(keyID, secret)
	}
	if err != nil {
		return fmt.Errorf("unlock Matrix secret storage: %w", err)
	}
	backupBytes, err := mach.GetDecryptedAccountData(ctx, event.AccountDataMegolmBackupKey, key)
	if err != nil {
		return fmt.Errorf("decrypt Matrix key-backup secret: %w", err)
	}
	backupKey, err := backup.MegolmBackupKeyFromBytes(backupBytes)
	if err != nil {
		return fmt.Errorf("decode Matrix key-backup secret: %w", err)
	}
	return r.restoreWithBackupKey(ctx, backupKey)
}

// restoreWithBackupKey imports the latest backup and caches its decryption key
// so later syncs can fetch room keys that other devices add to the backup. The
// recovery key or passphrase that unlocked it is not retained.
func (r *Runtime) restoreWithBackupKey(ctx context.Context, backupKey *backup.MegolmBackupKey) error {
	mach := r.olmMachine()
	if mach == nil {
		return errors.New("matrix crypto is unavailable")
	}
	if _, err := mach.DownloadAndStoreLatestKeyBackup(ctx, backupKey); err != nil {
		return fmt.Errorf("restore Matrix key backup: %w", err)
	}
	encoded := base64.RawStdEncoding.EncodeToString(backupKey.Bytes())
	if err := mach.CryptoStore.PutSecret(ctx, cachedBackupKeySecret, encoded); err != nil {
		return fmt.Errorf("cache Matrix key-backup key: %w", err)
	}
	return nil
}

// errBackupKeyReplaced reports that the server's current backup was not
// created with the cached key, so it cannot decrypt the backup's sessions.
var errBackupKeyReplaced = errors.New("matrix key backup was replaced by one the cached backup key cannot read")

// loadBackup reads the cached backup key once per runtime and checks that it
// still matches the server's latest backup version. A missing key or backup
// leaves backup fetching disabled without error. The version request is made
// once per runtime: a failure is returned to the first caller, and backup
// fetching stays disabled for the rest of the run.
func (r *Runtime) loadBackup(ctx context.Context) error {
	if r.backupLoaded {
		return nil
	}
	err := r.readBackupVersion(ctx)
	if ctx.Err() == nil {
		r.backupLoaded = true
	}
	return err
}

func (r *Runtime) readBackupVersion(ctx context.Context) error {
	mach := r.olmMachine()
	if mach == nil {
		return nil
	}
	encoded, err := mach.CryptoStore.GetSecret(ctx, cachedBackupKeySecret)
	if err != nil {
		return fmt.Errorf("load cached Matrix key-backup key: %w", err)
	}
	if encoded == "" {
		return nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("decode cached Matrix key-backup key: %w", err)
	}
	key, err := backup.MegolmBackupKeyFromBytes(raw)
	if err != nil {
		return fmt.Errorf("decode cached Matrix key-backup key: %w", err)
	}
	info, err := r.Client.GetKeyBackupLatestVersion(ctx)
	if errors.Is(err, mautrix.MNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load Matrix key-backup version: %w", err)
	}
	publicKey := id.Ed25519(base64.RawStdEncoding.EncodeToString(key.PublicKey().Bytes()))
	if info.Algorithm != id.KeyBackupAlgorithmMegolmBackupV1 || info.AuthData.PublicKey != publicKey {
		return errBackupKeyReplaced
	}
	r.backupKey, r.backupVersion = key, info.Version
	return nil
}

// fetchBackupSession imports the room key for one undecryptable event from the
// server-side backup. It does nothing when the session is already known, no
// backup key is cached, or the backup does not hold the session.
func (r *Runtime) fetchBackupSession(ctx context.Context, evt *event.Event) error {
	mach := r.olmMachine()
	if mach == nil || evt == nil || evt.RoomID == "" {
		return nil
	}
	content, ok := evt.Content.Parsed.(*event.EncryptedEventContent)
	if !ok || content.SessionID == "" {
		return nil
	}
	if known, err := mach.CryptoStore.GetGroupSession(ctx, evt.RoomID, content.SessionID); err == nil && known != nil {
		return nil
	}
	key := backupSessionKey{evt.RoomID, content.SessionID}
	if _, tried := r.backupSessions[key]; tried {
		return nil
	}
	err := r.fetchBackupSessionOnce(ctx, mach, key)
	if ctx.Err() == nil {
		if r.backupSessions == nil {
			r.backupSessions = map[backupSessionKey]struct{}{}
		}
		r.backupSessions[key] = struct{}{}
	}
	return err
}

func (r *Runtime) fetchBackupSessionOnce(ctx context.Context, mach *crypto.OlmMachine, key backupSessionKey) error {
	if err := r.loadBackup(ctx); err != nil {
		return err
	}
	if r.backupKey == nil {
		return nil
	}
	data, err := r.Client.GetKeyBackupForRoomAndSession(ctx, r.backupVersion, key.roomID, key.sessionID)
	if errors.Is(err, mautrix.MNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("fetch Matrix backup key for session %s: %w", key.sessionID, err)
	}
	sessionData, err := data.SessionData.Decrypt(r.backupKey)
	if err != nil {
		return fmt.Errorf("decrypt Matrix backup key for session %s: %w", key.sessionID, err)
	}
	if _, err := mach.ImportRoomKeyFromBackup(ctx, r.backupVersion, key.roomID, key.sessionID, sessionData); err != nil {
		return fmt.Errorf("import Matrix backup key for session %s: %w", key.sessionID, err)
	}
	return nil
}

// ProcessSync imports to-device keys and room state before timeline events are decrypted.
func (r *Runtime) ProcessSync(ctx context.Context, resp *mautrix.RespSync, since string) {
	if r == nil || r.Crypto == nil {
		return
	}
	r.Crypto.Machine().ProcessSyncResponse(ctx, resp, since)
}
