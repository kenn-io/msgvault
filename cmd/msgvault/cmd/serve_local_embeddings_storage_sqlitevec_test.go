//go:build sqlite_vec

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

func TestLocalEmbeddingsActivationKeepsBuilderStorage(t *testing.T) {
	for _, storage := range []string{"separate home and config", "saved path after activation"} {
		t.Run(storage, func(t *testing.T) {
			f := newLocalActivationFixture(t, "", "")
			want := 1
			if storage == "separate home and config" {
				content, err := os.ReadFile(f.cfg.ConfigFilePath())
				require.NoError(t, err)
				path := filepath.Join(t.TempDir(), "config.toml")
				require.NoError(t, os.WriteFile(path, content, 0o600))
				snapshot, err := config.ReadConfigFile(path)
				require.NoError(t, err)
				_, err = config.EditConfigTables(path, snapshot.ETag, []config.TableEdit{{Path: []string{"vector"}, Values: map[string]any{"db_path": "relative-vectors.db"}}})
				require.NoError(t, err)
				snapshot, err = config.ReadConfigFile(path)
				require.NoError(t, err)
				f.cfg, err = config.LoadConfigFile(snapshot, f.cfg.HomeDir)
				require.NoError(t, err)
				f.state.cfg = f.cfg
			}
			f.start(t)
			if storage == "saved path after activation" {
				require.NoError(t, f.controller.Enable(f.ctx, func(api.LocalEmbeddingsStatus) {}))
				snapshot, err := config.ReadConfigFile(f.cfg.ConfigFilePath())
				require.NoError(t, err)
				_, err = config.EditConfigTables(f.cfg.ConfigFilePath(), snapshot.ETag, []config.TableEdit{{Path: []string{"vector"}, Values: map[string]any{"db_path": filepath.Join(t.TempDir(), "replacement-vectors.db")}}})
				require.NoError(t, err)
				_, _, err = testutil.CreateIndexedSourceMessage(t.Context(), f.store, "archive@example.test", "later", "Later orchard", "Another orchard message.")
				require.NoError(t, err)
				want = 2
			}
			require.NoError(t, f.controller.Enable(f.ctx, func(api.LocalEmbeddingsStatus) {}))
			results, _, err := f.handle.vf.HybridEngine.Search(f.ctx, hybrid.SearchRequest{Mode: hybrid.ModeVector, FreeText: "orchard", Limit: 10})
			require.NoError(t, err)
			assert.Len(t, results, want)
		})
	}
}
