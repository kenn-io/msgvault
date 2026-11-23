//go:build linux

package peoplesweep

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// This opt-in ceremony uses the production model-less enrollment constructor.
// The short-lived URL and code are shown only in the operator's test output.
func TestCodexPinnedArtifactDeviceEnrollment(t *testing.T) {
	requireChecks := require.New(t)
	if os.Getenv("MSGVAULT_CODEX_ENROLL_NOW") != "1" {
		t.Skip("device enrollment requires an explicit operator start")
	}
	artifact := os.Getenv("MSGVAULT_CODEX_PINNED_EXECUTABLE")
	authHome := os.Getenv("MSGVAULT_CODEX_AUTH_HOME")
	requireChecks.NotEmpty(artifact)
	requireChecks.NotEmpty(authHome)
	client, err := NewCodexEnrollmentClient(artifact, authHome, 10*time.Minute)
	requireChecks.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	err = client.StartDeviceLogin(ctx, func(login DeviceLogin) error {
		t.Logf("Official verification URL: %s", login.VerificationURL)
		t.Logf("One-time user code: %s", login.UserCode)
		t.Logf("Local login deadline: %s", login.ExpiresAt.UTC().Format(time.RFC3339))
		return nil
	})
	requireChecks.NoError(err)
	contents, _, err := readPrivateCodexAuth(authHome)
	requireChecks.NoError(err)
	_, err = codexAccountIdentityFromAuth(contents)
	requireChecks.NoError(err)
}

// newCodexPinnedProbeStarter runs the probes through the production
// Bubblewrap starter, with a freshly built bridge unless
// MSGVAULT_CODEX_TEST_DEFAULT_BRIDGE=1 selects the installed one.
func newCodexPinnedProbeStarter(ctx context.Context, t *testing.T) CommandStarter {
	t.Helper()
	if os.Getenv("MSGVAULT_CODEX_TEST_DEFAULT_BRIDGE") == "1" {
		return NewCodexCommandStarter()
	}
	bridge := buildCodexProxyBridge(ctx, t)
	bridgeDigest, err := hashCodexExecutable(bridge)
	require.NoError(t, err)
	return newCodexCommandStarterWithBridge(bridge, bridgeDigest)
}

func buildCodexProxyBridge(ctx context.Context, t *testing.T) string {
	t.Helper()
	bridge := filepath.Join(t.TempDir(), "msgvault-codex-bridge")
	// This read makes Go's test cache rerun the test when the child's inputs change.
	_, err := os.ReadDir(filepath.Join("..", "..", "cmd", "msgvault-codex-bridge"))
	require.NoError(t, err)
	command := exec.CommandContext(ctx, "go", "build", "-o", bridge, "go.kenn.io/msgvault/cmd/msgvault-codex-bridge")
	command.Env = append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off")
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	require.NoError(t, os.Chmod(bridge, 0o700))
	return bridge
}
