package cmd

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	xapi "go.kenn.io/msgvault/internal/x"
)

func TestRegisterXCleanupFailureCannotReinstallRotatedStage(t *testing.T) {
	testXRegistrationRecovery(t, false)
}

func TestRegisterXPublicationFailureCannotReinstallRotatedStage(t *testing.T) {
	testXRegistrationRecovery(t, true)
}

func testXRegistrationRecovery(t *testing.T, beforePublication bool) {
	t.Helper()
	if !beforePublication && runtime.GOOS == "windows" {
		t.Skip("real directory deletion failure uses Unix permissions")
	}
	cfg := xTestConfig(t)
	st := testutil.NewTestStore(t)
	main := xapi.NewTokenManager(cfg.TokensDir())
	stage := xapi.NewTokenManager(xStagedTokensDir(cfg))
	record := xTestRecord("new-access")
	record.GrantedScopes = []string{"tweet.read", "users.read", "offline.access"}
	require.NoError(t, stage.Save(t.Context(), record))
	stageDir := xStagedTokensDir(cfg)
	t.Cleanup(func() { assert.NoError(t, os.Chmod(stageDir, 0700)) })
	if !beforePublication {
		// Confirm the current platform/user enforces the exact failure used below.
		probe := filepath.Join(stageDir, "permission-probe")
		require.NoError(t, os.WriteFile(probe, []byte("synthetic"), 0600))
		require.NoError(t, os.Chmod(stageDir, 0500))
		removeErr := os.Remove(probe)
		require.NoError(t, os.Chmod(stageDir, 0700))
		if removeErr == nil {
			t.Skip("current user bypasses directory write permissions")
		}
		require.ErrorIs(t, removeErr, os.ErrPermission)
		require.NoError(t, os.Remove(probe))
	}
	var mu sync.Mutex
	var refreshTokens, bearers []string
	failCleanup := !beforePublication
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			require.NoError(t, r.ParseForm())
			assert.Equal(t, "refresh_token", r.Form.Get("grant_type"))
			token := r.Form.Get("refresh_token")
			mu.Lock()
			refreshTokens = append(refreshTokens, token)
			count := len(refreshTokens)
			mu.Unlock()
			if token == "example-refresh" && count > 1 {
				w.WriteHeader(400)
				fmt.Fprint(w, `{"error":"invalid_grant"}`)
				return
			}
			switch token {
			case "example-refresh":
				fmt.Fprint(w, `{"access_token":"rotated-access-2","refresh_token":"rotated-refresh-2","token_type":"bearer","expires_in":7200,"scope":"tweet.read users.read offline.access"}`)
			case "rotated-refresh-2":
				fmt.Fprint(w, `{"access_token":"rotated-access-3","refresh_token":"rotated-refresh-3","token_type":"bearer","expires_in":7200,"scope":"tweet.read users.read offline.access"}`)
			default:
				assert.Fail(t, "unexpected synthetic refresh token")
				w.WriteHeader(400)
			}
			return
		}
		assert.Equal(t, "/2/users/me", r.URL.Path)
		mu.Lock()
		bearers = append(bearers, r.Header.Get("Authorization"))
		shouldFail := failCleanup
		failCleanup = false
		mu.Unlock()
		if shouldFail {
			assert.NoError(t, os.Chmod(stageDir, 0500))
		}
		// Old access remains valid after refresh, matching the proposed sequence.
		fmt.Fprint(w, `{"data":{"id":"1001","username":"example_user","name":"Example Author","protected":false}}`)
	}))
	t.Cleanup(provider.Close)
	deps := xCommandDeps{apiBaseURL: provider.URL + "/2", httpClient: provider.Client(), oauthOptions: xapi.OAuthOptions{TokenURL: provider.URL + "/token"}}
	source, err := st.GetOrCreateSource("x", "1001")
	require.NoError(t, err)
	initial, err := st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	generation := int64(100)
	credentialError := "invalid_grant"
	walk := store.XWalk{Endpoint: "own", Fingerprint: "recovery-window", Token: "page-saved"}
	spend := []store.XSpendBucket{{Month: "2026-10", SpentMicros: 1234}}
	_, err = st.UpdateXControlContext(t.Context(), source.ID, initial.Revision, store.XControlPatch{OwnerID: "1001", Generation: &generation, CredentialError: &credentialError, Own: &walk, SpendBuckets: &spend})
	require.NoError(t, err)
	if beforePublication {
		if st.IsPostgreSQL() {
			_, err = st.DB().Exec(`CREATE FUNCTION fail_x_registration_publish_fn() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic publication interruption'; END $$; CREATE TRIGGER fail_x_registration_publish BEFORE UPDATE OF sync_cursor ON sources FOR EACH ROW EXECUTE FUNCTION fail_x_registration_publish_fn()`)
		} else {
			_, err = st.DB().Exec(`CREATE TRIGGER fail_x_registration_publish BEFORE UPDATE OF sync_cursor ON sources BEGIN SELECT RAISE(ABORT, 'synthetic publication interruption'); END`)
		}
		require.NoError(t, err)
	}
	_, err = registerXAccountWithDeps(t.Context(), st, cfg, "1001", deps)
	var coded *api.CLIRunCodedError
	if beforePublication {
		require.ErrorContains(t, err, "synthetic publication interruption")
		if st.IsPostgreSQL() {
			_, err = st.DB().Exec(`DROP TRIGGER fail_x_registration_publish ON sources; DROP FUNCTION fail_x_registration_publish_fn()`)
		} else {
			_, err = st.DB().Exec(`DROP TRIGGER fail_x_registration_publish`)
		}
		require.NoError(t, err)
	} else {
		require.ErrorAs(t, err, &coded)
		require.Equal(t, "staged_cleanup_failed", coded.Code)
	}
	require.NoError(t, os.Chmod(stageDir, 0700))
	installed, err := main.Load(t.Context(), "1001")
	require.NoError(t, err)
	source, err = st.GetSourceByTypeAndIdentifier("x", "1001")
	require.NoError(t, err)
	control, err := st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	if beforePublication {
		assert.Equal(t, int64(100), control.Generation)
	} else {
		assert.Equal(t, installed.Generation, control.Generation)
	}
	_, err = stage.Load(t.Context(), "1001")
	require.NoError(t, err)
	// Real ordinary refresh persists R2 and advances the Store generation.
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source.ID)
	require.NoError(t, err)
	manager := deps.manager(cfg.TokensDir())
	credentials := manager.Credentials("1001", provider.Client())
	var rotated xapi.CredentialSnapshot
	err = manager.WithLifecycleLock(t.Context(), "1001", func() error {
		var refreshErr error
		rotated, refreshErr = credentials.Refresh(t.Context(), installed.Generation)
		return refreshErr
	})
	require.NoError(t, err)
	if !beforePublication {
		_, err = st.UpdateXControlContext(t.Context(), source.ID, control.Revision, store.XControlPatch{OwnerID: "1001", Generation: &rotated.Generation})
		require.NoError(t, err)
	}
	require.NoError(t, execution.Release())
	beforeRetry, err := main.Load(t.Context(), "1001")
	require.NoError(t, err)
	require.Equal(t, "rotated-refresh-2", beforeRetry.RefreshToken())
	_, err = registerXAccountWithDeps(t.Context(), st, cfg, "1001", deps)
	require.NoError(t, err)
	afterRetry, err := main.Load(t.Context(), "1001")
	require.NoError(t, err)
	assert.Equal(t, "rotated-access-2", afterRetry.AccessToken())
	assert.Equal(t, "rotated-refresh-2", afterRetry.RefreshToken())
	assert.NotContains(t, afterRetry.GrantedScopes, "tweet.write")
	afterControl, err := st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	assert.Equal(t, afterRetry.Generation, afterControl.Generation)
	assert.Empty(t, afterControl.CredentialError)
	assert.Equal(t, &walk, afterControl.Own)
	assert.Equal(t, spend, afterControl.SpendBuckets)
	_, err = stage.Load(t.Context(), "1001")
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = manager.Credentials("1001", provider.Client()).Refresh(context.Background(), afterRetry.Generation)
	assert.NoError(t, err)
	mu.Lock()
	assert.Equal(t, []string{"example-refresh", "rotated-refresh-2"}, refreshTokens)
	assert.Equal(t, []string{"Bearer new-access", "Bearer rotated-access-2"}, bearers)
	mu.Unlock()
	// A new verified authorization still replaces the current grant, even when
	// its stage counter restarts at one after the old stage was consumed.
	fresh := xapi.NewTokenRecord("1001", "example-client", xapi.PublicClient, "fresh-access", "fresh-refresh", "")
	fresh.GrantedScopes = record.GrantedScopes
	fresh.Expiry = record.Expiry
	require.NoError(t, stage.Save(t.Context(), fresh))
	replacement, err := registerXAccountWithDeps(t.Context(), st, cfg, "1001", deps)
	require.NoError(t, err)
	assert.Equal(t, source.ID, replacement.ID)
	final, err := main.Load(t.Context(), "1001")
	require.NoError(t, err)
	assert.Equal(t, "fresh-refresh", final.RefreshToken())
	finalControl, err := st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	assert.Equal(t, spend, finalControl.SpendBuckets)
	assert.Equal(t, &walk, finalControl.Own)
}
