package daemonclient

import (
	"context"
	"errors"

	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// BackupFreezeBegin asks the daemon to open a backup freeze window: the
// daemon's operation gate is held from this call until BackupFreezeEnd (or
// the daemon's watchdog auto-releases it) so no other daemon-owned mutation
// runs while the backup subprocess checkpoints and pins its own SQLite
// session. The returned token must be passed to BackupFreezeEnd.
//
// Like other daemonclient mutation calls, APIResponse retries while the
// daemon reports its gate held by unrelated work; a second freeze already
// active surfaces as a plain (non-retried) error instead.
func (c *Client) BackupFreezeBegin(ctx context.Context) (string, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.BeginBackupFreezeResp, error) {
		return client.BeginBackupFreezeWithResponse(ctx)
	})
	if err != nil {
		return "", err
	}
	if resp.JSON200 == nil || resp.JSON200.Token == "" {
		return "", errors.New("backup freeze begin response missing token")
	}
	return resp.JSON200.Token, nil
}

// BackupFreezeEnd closes a backup freeze window opened by BackupFreezeBegin,
// releasing the daemon's operation gate. An error means the daemon's window
// was not open with that token (e.g. its watchdog already fired); the caller
// must treat its backup as unfrozen and fail rather than proceed silently.
func (c *Client) BackupFreezeEnd(ctx context.Context, token string) error {
	_, err := APIResponse(c, func(client *apiclient.Client) (*generated.EndBackupFreezeResp, error) {
		return client.EndBackupFreezeWithResponse(ctx, &generated.EndBackupFreezeRequestOptions{
			Body: &generated.EndBackupFreezeBody{Token: token},
		})
	})
	return err
}
