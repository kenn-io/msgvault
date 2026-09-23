//go:build linux

package peoplesweep

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type codexEnrollmentClient struct {
	driver   *CodexAppServerDriver
	launcher codexBoundLauncher
	authHome string
}

// NewCodexEnrollmentClient uses the enrollment-only executable pin and built helper.
// authHome must be a dedicated, owner-only directory created by the daemon.
func NewCodexEnrollmentClient(executable, authHome string, timeout time.Duration) (CodexEnrollment, error) {
	return NewCodexEnrollmentClientWithDependencies(executable, authHome, timeout, NewCodexCommandStarter(), codexEnrollmentIsolationGate{})
}

// NewCodexEnrollmentClientWithDependencies permits an explicit launch gate and
// starter for containment probes. Production callers use NewCodexEnrollmentClient.
func NewCodexEnrollmentClientWithDependencies(executable, authHome string, timeout time.Duration, starter CommandStarter, gate CodexIsolationGate) (CodexEnrollment, error) {
	if executable == "" || timeout <= 0 || timeout > 15*time.Minute {
		return nil, errors.New("codex enrollment requires executable and bounded timeout")
	}
	if starter == nil || gate == nil {
		return nil, errors.New("codex enrollment requires command starter and isolation gate")
	}
	if err := codexPrivateAuthHome(authHome); err != nil {
		return nil, err
	}
	launcher := codexBoundLauncher{starter: starter, gate: gate, proxy: defaultCodexServiceProxy()}
	if launcher.proxy == nil {
		return nil, ErrCodexProxyUnreleased
	}
	driver := &CodexAppServerDriver{
		Config:   ProviderConfig{Executable: executable, ExecutionBoundary: CodexExecutionBoundaryV1, RequestTimeout: timeout},
		launcher: launcher, authHome: authHome,
	}
	return &codexEnrollmentClient{driver: driver, launcher: launcher, authHome: authHome}, nil
}

func (e *codexEnrollmentClient) StartDeviceLogin(ctx context.Context, present func(DeviceLogin) error) (retErr error) {
	if present == nil {
		return errors.New("codex device login presenter is required")
	}
	operationCtx, cancel := context.WithTimeout(ctx, e.driver.Config.RequestTimeout)
	defer cancel()
	releaseAuth, err := lockCodexAuthOperation(operationCtx, e.authHome)
	if err != nil {
		return err
	}
	defer releaseAuth()
	attestation, err := e.driver.attest(operationCtx)
	if err != nil {
		return err
	}
	process, err := e.launcher.StartLogin(operationCtx, attestation, e.authHome)
	if err != nil {
		return errors.Join(fmt.Errorf("start codex enrollment process: %w", err), attestation.Close())
	}
	client := &CodexRPCClient{Process: process}
	defer func() {
		retErr = errors.Join(retErr, finishCodexProcess(operationCtx, process, client, retErr != nil), attestation.Close())
	}()
	if err := runCodexDeviceLogin(operationCtx, client, present); err != nil {
		return err
	}
	if err := operationCtx.Err(); err != nil {
		return err
	}
	owned, ok := process.(*codexOwnedProcess)
	if !ok {
		return errors.New("codex enrollment process has no owned work root")
	}
	return owned.CommitLoginAuth(e.authHome)
}

func (e *codexEnrollmentClient) ListModels(ctx context.Context) ([]CodexModel, error) {
	return e.driver.ListModels(ctx)
}
