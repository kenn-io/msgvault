//go:build linux

package peoplesweep

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexEnrollmentPinAllowsOnlyModelLessEnrollment(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	artifact := os.Getenv("MSGVAULT_CODEX_PINNED_EXECUTABLE")
	if artifact == "" {
		t.Skip("set MSGVAULT_CODEX_PINNED_EXECUTABLE to the pinned native artifact")
	}
	authHome := t.TempDir()
	requireChecks.NoError(os.Chmod(authHome, 0o700))
	client, err := NewCodexEnrollmentClient(artifact, authHome, time.Minute)
	requireChecks.NoError(err)
	enrollment, ok := client.(*codexEnrollmentClient)
	requireChecks.True(ok)
	attestation, err := enrollment.driver.attest(context.Background())
	requireChecks.NoError(err)
	assertChecks.Equal("codex-cli 0.156.0", attestation.Version)
	requireChecks.NoError(attestation.Close())
	assertChecks.Empty(releasedCodexAttestations)
	_, err = NewReleasedCodexIsolationGate().Verify(context.Background(), artifact, CodexExecutionBoundaryV1)
	requireChecks.ErrorIs(err, ErrCodexIsolationUnreleased)
}

func TestCodexEnrollmentPinnedLauncherStartsEmptyLoginSession(t *testing.T) {
	requireChecks := require.New(t)
	artifact := os.Getenv("MSGVAULT_CODEX_PINNED_EXECUTABLE")
	if artifact == "" {
		t.Skip("set MSGVAULT_CODEX_PINNED_EXECUTABLE to the pinned native artifact")
	}
	authHome := t.TempDir()
	requireChecks.NoError(os.Chmod(authHome, 0o700))
	client, err := NewCodexEnrollmentClient(artifact, authHome, time.Minute)
	requireChecks.NoError(err)
	enrollment, ok := client.(*codexEnrollmentClient)
	requireChecks.True(ok)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	attestation, err := enrollment.driver.attest(ctx)
	requireChecks.NoError(err)
	defer func() { require.NoError(t, attestation.Close()) }()
	process, err := enrollment.launcher.StartLogin(ctx, attestation, authHome)
	requireChecks.NoError(err)
	rpc := &CodexRPCClient{Process: process}
	defer func() { require.NoError(t, finishCodexProcess(ctx, process, rpc, true)) }()
	var initialized map[string]any
	requireChecks.NoError(rpc.Call(ctx, "initialize", codexInitializeRequest().Params, &initialized))
	requireChecks.NoError(rpc.Notify(ctx, "initialized", nil))
	assert.NoFileExists(t, authHome+"/auth.json")
}
