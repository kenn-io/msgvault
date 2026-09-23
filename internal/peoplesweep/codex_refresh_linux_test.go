//go:build linux

package peoplesweep

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexRefreshRejectsChangedSourceOrUnprivateCandidate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mutate     func(string, string) error
		wantError  error
		wantSource bool
	}{
		{name: "source replaced", mutate: func(authHome, _ string) error {
			return os.WriteFile(filepath.Join(authHome, "auth.json"), syntheticCodexProbeAuth("user-two", "workspace-one", "external"), 0o600)
		}, wantError: ErrCodexAuthSourceChanged, wantSource: true},
		{name: "candidate public", mutate: func(_, workRoot string) error {
			return os.Chmod(filepath.Join(workRoot, ".codex", "auth.json"), 0o644)
		}, wantError: ErrCodexAuthRefreshUnsafe},
		{name: "candidate symlink", mutate: func(_, workRoot string) error {
			candidate := filepath.Join(workRoot, ".codex", "auth.json")
			if err := os.Remove(candidate); err != nil {
				return err
			}
			return os.Symlink(filepath.Join(workRoot, "refresh.json"), candidate)
		}, wantError: ErrCodexAuthRefreshUnsafe},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireChecks := require.New(t)
			authHome := t.TempDir()
			workRoot := t.TempDir()
			requireChecks.NoError(os.Chmod(authHome, 0o700))
			requireChecks.NoError(os.Chmod(workRoot, 0o700))
			requireChecks.NoError(os.Mkdir(filepath.Join(workRoot, ".codex"), 0o700))
			initial := syntheticCodexProbeAuth("user-one", "workspace-one", "old")
			refreshed := syntheticCodexProbeAuth("user-one", "workspace-one", "new")
			requireChecks.NoError(os.WriteFile(filepath.Join(authHome, "auth.json"), initial, 0o600))
			requireChecks.NoError(os.WriteFile(filepath.Join(workRoot, ".codex", "auth.json"), initial, 0o600))
			state, err := prepareCodexRefresh(authHome, workRoot)
			requireChecks.NoError(err)
			requireChecks.NoError(os.WriteFile(filepath.Join(workRoot, ".codex", "auth.json"), refreshed, 0o600))
			requireChecks.NoError(tc.mutate(authHome, workRoot))
			requireChecks.ErrorIs(state.commit(), tc.wantError)
			contents, err := os.ReadFile(filepath.Join(authHome, "auth.json"))
			requireChecks.NoError(err)
			want := initial
			if tc.wantSource {
				want = syntheticCodexProbeAuth("user-two", "workspace-one", "external")
			}
			assert.Equal(t, sha256.Sum256(want), sha256.Sum256(contents))
		})
	}
}
