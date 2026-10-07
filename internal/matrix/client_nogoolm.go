//go:build !goolm

package matrix

import (
	"context"
	"errors"
	"fmt"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
)

// ErrEncryptionUnavailable reports a build without the goolm tag. Such a
// build has no Olm machine, so it cannot keep the device's room keys; Open
// refuses to sync rather than consume to-device key events it cannot store.
var ErrEncryptionUnavailable = errors.New("matrix support requires a msgvault build with the goolm tag (end-to-end encryption)")

// Runtime owns a mautrix client. Builds without the goolm tag have no Olm
// machine; see client_goolm.go for the encrypting runtime.
type Runtime struct {
	Client *mautrix.Client
	// decryptEvent is a test seam for exercising importer behavior without a
	// real Olm machine.
	decryptEvent func(context.Context, *event.Event) (*event.Event, error)
}

func (r *Runtime) canDecrypt() bool {
	return r != nil && r.decryptEvent != nil
}

func (r *Runtime) decrypt(ctx context.Context, evt *event.Event) (*event.Event, error) {
	if r == nil || r.decryptEvent == nil {
		return nil, ErrEncryptionUnavailable
	}
	decrypted, err := r.decryptEvent(ctx, evt)
	if err != nil {
		return nil, fmt.Errorf("decrypt Matrix event: %w", err)
	}
	return decrypted, nil
}

// Open reports ErrEncryptionUnavailable in builds without the goolm tag.
func Open(_ context.Context, creds Credentials, _ string) (*Runtime, error) {
	if err := validateHomeserverURL(creds.Homeserver); err != nil {
		return nil, err
	}
	return nil, ErrEncryptionUnavailable
}

func (r *Runtime) Close() error { return nil }

// RestoreKeyBackup reports ErrEncryptionUnavailable in builds without the
// goolm tag.
func (r *Runtime) RestoreKeyBackup(context.Context, string, bool) error {
	return ErrEncryptionUnavailable
}

func (r *Runtime) fetchBackupSession(context.Context, *event.Event) error { return nil }

// ProcessSync is a no-op without an Olm machine.
func (r *Runtime) ProcessSync(context.Context, *mautrix.RespSync, string) {}
