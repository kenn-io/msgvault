//go:build linux

package peoplesweep_test

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func TestCodexEnrollmentCommitsOnlySuccessfulLoginAndListsWithDedicatedHome(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	authHome := t.TempDir()
	requireChecks.NoError(os.Chmod(authHome, 0o700))
	syntheticAuth := syntheticCodexAuth("user-one", "workspace-one", "refresh")
	start := func(reader *bufio.Reader, stdout io.Writer, _ io.Writer) error {
		if _, err := reader.ReadBytes('\n'); err != nil {
			return fmt.Errorf("read initialize request: %w", err)
		}
		if err := writeRPCFrame(stdout, map[string]any{"id": 1, "result": map[string]any{}}); err != nil {
			return err
		}
		if _, err := reader.ReadBytes('\n'); err != nil {
			return fmt.Errorf("read initialized notification: %w", err)
		}
		if _, err := reader.ReadBytes('\n'); err != nil {
			return fmt.Errorf("read device login request: %w", err)
		}
		if err := writeRPCFrame(stdout, map[string]any{"id": 2, "result": map[string]any{
			"type": "chatgptDeviceCode", "loginId": "synthetic-login",
			"verificationUrl": "https://auth.example.test/device", "userCode": "ABCD-1234",
		}}); err != nil {
			return err
		}
		return nil
	}
	var workRoot string
	launches := 0
	starter := &recordingCodexStarter{t: t, inspect: func(dir string) {
		launches++
		workRoot = dir
		if launches == 2 {
			contents, err := os.ReadFile(filepath.Join(dir, ".codex", "auth.json"))
			require.NoError(t, err)
			assert.Equal(t, syntheticAuth, contents)
		}
	}, scripts: []func(*bufio.Reader, io.Writer, io.Writer) error{
		func(reader *bufio.Reader, stdout, stderr io.Writer) error {
			if err := start(reader, stdout, stderr); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(workRoot, ".codex", "auth.json"), syntheticAuth, 0o600); err != nil {
				return err
			}
			return writeRPCFrame(stdout, map[string]any{"method": "account/login/completed", "params": map[string]any{"success": true, "loginId": "synthetic-login"}})
		},
		func(reader *bufio.Reader, stdout, _ io.Writer) error {
			if _, err := reader.ReadBytes('\n'); err != nil {
				return fmt.Errorf("read model initialize request: %w", err)
			}
			if err := writeRPCFrame(stdout, map[string]any{"id": 1, "result": map[string]any{}}); err != nil {
				return err
			}
			if _, err := reader.ReadBytes('\n'); err != nil {
				return fmt.Errorf("read model initialized notification: %w", err)
			}
			if _, err := reader.ReadBytes('\n'); err != nil {
				return fmt.Errorf("read model list request: %w", err)
			}
			return writeRPCFrame(stdout, map[string]any{"id": 2, "result": map[string]any{
				"data": []any{map[string]any{"id": "gpt-test", "model": "gpt-test", "displayName": "Test Model", "defaultReasoningEffort": "medium", "supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "medium", "description": "Balanced"}}}}, "nextCursor": nil,
			}})
		},
	}}
	client, err := peoplesweep.NewCodexEnrollmentClientWithDependencies("codex", authHome, time.Second, starter, &recordingCodexGate{})
	requireChecks.NoError(err)
	err = client.StartDeviceLogin(t.Context(), func(login peoplesweep.DeviceLogin) error {
		assert.Equal(t, "ABCD-1234", login.UserCode)
		assert.NoFileExists(t, filepath.Join(authHome, "auth.json"))
		return nil
	})
	requireChecks.NoError(err)
	assertChecks.NoDirExists(starter.records[0].dir)
	contents, err := os.ReadFile(filepath.Join(authHome, "auth.json"))
	requireChecks.NoError(err)
	assertChecks.Equal(syntheticAuth, contents)
	models, err := client.ListModels(t.Context())
	requireChecks.NoError(err)
	assertChecks.Equal([]peoplesweep.CodexModel{{ID: "gpt-test", DisplayName: "Test Model", DefaultReasoningEffort: "medium", SupportedEfforts: []string{"medium"}}}, models)
	assertChecks.Equal(int64(2), starter.proxyStarts.Load())
}

func TestCodexEnrollmentRejectsPublicAuthHome(t *testing.T) {
	authHome := t.TempDir()
	require.NoError(t, os.Chmod(authHome, 0o755))
	client, err := peoplesweep.NewCodexEnrollmentClientWithDependencies("codex", authHome, time.Second, &recordingCodexStarter{t: t}, &recordingCodexGate{})
	require.ErrorContains(t, err, "private")
	assert.Nil(t, client)
}

func TestCodexEnrollmentFailedLoginLeavesNoDedicatedCredential(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	authHome := t.TempDir()
	requireChecks.NoError(os.Chmod(authHome, 0o700))
	starter := &recordingCodexStarter{t: t, scripts: []func(*bufio.Reader, io.Writer, io.Writer) error{
		func(reader *bufio.Reader, stdout, _ io.Writer) error {
			if _, err := reader.ReadBytes('\n'); err != nil {
				return fmt.Errorf("read failed-login initialize request: %w", err)
			}
			if err := writeRPCFrame(stdout, map[string]any{"id": 1, "result": map[string]any{}}); err != nil {
				return err
			}
			if _, err := reader.ReadBytes('\n'); err != nil {
				return fmt.Errorf("read failed-login initialized notification: %w", err)
			}
			if _, err := reader.ReadBytes('\n'); err != nil {
				return fmt.Errorf("read failed-login start request: %w", err)
			}
			return writeRPCFrame(stdout, map[string]any{"id": 2, "result": map[string]any{"type": "chatgptDeviceCode", "loginId": "synthetic-login", "verificationUrl": "https://auth.example.test/device", "userCode": "ABCD-1234"}})
		},
	}}
	client, err := peoplesweep.NewCodexEnrollmentClientWithDependencies("codex", authHome, time.Second, starter, &recordingCodexGate{})
	requireChecks.NoError(err)
	err = client.StartDeviceLogin(t.Context(), func(peoplesweep.DeviceLogin) error { return errors.New("presenter cancelled") })
	requireChecks.ErrorContains(err, "presenter cancelled")
	assertChecks.NoFileExists(filepath.Join(authHome, "auth.json"))
	assertChecks.NoDirExists(starter.records[0].dir)
}

func TestCodexEnrollmentAccountSwitchPreservesOldAuthUntilSuccessfulLogin(t *testing.T) {
	for _, tc := range []struct {
		name   string
		accept bool
	}{
		{name: "successful switch", accept: true},
		{name: "cancelled switch", accept: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertChecks := assert.New(t)
			requireChecks := require.New(t)
			authHome := t.TempDir()
			requireChecks.NoError(os.Chmod(authHome, 0o700))
			oldAuth := syntheticCodexAuth("user-one", "workspace-one", "old")
			newAuth := syntheticCodexAuth("user-two", "workspace-two", "new")
			requireChecks.NoError(os.WriteFile(filepath.Join(authHome, "auth.json"), oldAuth, 0o600))
			var workRoot string
			starter := &recordingCodexStarter{t: t, inspect: func(dir string) {
				workRoot = dir
				assert.NoFileExists(t, filepath.Join(dir, ".codex", "auth.json"))
			}, scripts: []func(*bufio.Reader, io.Writer, io.Writer) error{
				func(reader *bufio.Reader, stdout, _ io.Writer) error {
					if _, err := reader.ReadBytes('\n'); err != nil {
						return fmt.Errorf("read repeat-login initialize request: %w", err)
					}
					if err := writeRPCFrame(stdout, map[string]any{"id": 1, "result": map[string]any{}}); err != nil {
						return err
					}
					if _, err := reader.ReadBytes('\n'); err != nil {
						return fmt.Errorf("read repeat-login initialized notification: %w", err)
					}
					if _, err := reader.ReadBytes('\n'); err != nil {
						return fmt.Errorf("read repeat-login start request: %w", err)
					}
					if err := writeRPCFrame(stdout, map[string]any{"id": 2, "result": map[string]any{"type": "chatgptDeviceCode", "loginId": "synthetic-login", "verificationUrl": "https://auth.example.test/device", "userCode": "ABCD-1234"}}); err != nil {
						return err
					}
					if !tc.accept {
						return nil
					}
					if err := os.WriteFile(filepath.Join(workRoot, ".codex", "auth.json"), newAuth, 0o600); err != nil {
						return err
					}
					return writeRPCFrame(stdout, map[string]any{"method": "account/login/completed", "params": map[string]any{"success": true, "loginId": "synthetic-login"}})
				},
			}}
			client, err := peoplesweep.NewCodexEnrollmentClientWithDependencies("codex", authHome, time.Second, starter, &recordingCodexGate{})
			requireChecks.NoError(err)
			err = client.StartDeviceLogin(t.Context(), func(peoplesweep.DeviceLogin) error {
				contents, readErr := os.ReadFile(filepath.Join(authHome, "auth.json"))
				require.NoError(t, readErr)
				assert.Equal(t, oldAuth, contents)
				if !tc.accept {
					return errors.New("cancelled")
				}
				return nil
			})
			if tc.accept {
				requireChecks.NoError(err)
			} else {
				requireChecks.ErrorContains(err, "cancelled")
			}
			contents, err := os.ReadFile(filepath.Join(authHome, "auth.json"))
			requireChecks.NoError(err)
			if tc.accept {
				assertChecks.Equal(newAuth, contents)
			} else {
				assertChecks.Equal(oldAuth, contents)
			}
			info, err := os.Lstat(filepath.Join(authHome, "auth.json"))
			requireChecks.NoError(err)
			assertChecks.Equal(os.FileMode(0o600), info.Mode().Perm())
			assertChecks.NoDirExists(workRoot)
		})
	}
}
