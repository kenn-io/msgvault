//go:build sqlite_vec

package cmd

import (
	"fmt"
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
	require := require.New(t)
	f := newLocalActivationFixture(t, "")
	content, err := os.ReadFile(f.cfg.ConfigFilePath())
	require.NoError(err)
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, content, 0o600))
	snapshot, err := config.ReadConfigFile(path)
	require.NoError(err)
	_, err = config.EditConfigTables(path, snapshot.ETag, []config.TableEdit{{Path: []string{"vector"}, Values: map[string]any{"db_path": "relative-vectors.db"}}})
	require.NoError(err)
	snapshot, err = config.ReadConfigFile(path)
	require.NoError(err)
	f.cfg, err = config.LoadConfigFile(snapshot, f.cfg.HomeDir)
	require.NoError(err)
	f.state.cfg = f.cfg
	f.start(t)
	require.NoError(f.controller.Enable(f.ctx, func(api.LocalEmbeddingsStatus) {}))
	results, _, err := f.handle.vf.HybridEngine.Search(f.ctx, hybrid.SearchRequest{Mode: hybrid.ModeVector, FreeText: "orchard", Limit: 10})
	require.NoError(err)
	require.Len(results, 1)
}

func TestLocalEmbeddingsRefusesPendingArchiveStorageChange(t *testing.T) {
	for _, setting := range []struct {
		table, field string
		live         []bool
	}{
		{table: "data", field: "database_url", live: []bool{false}},
		{table: "vector", field: "db_path", live: []bool{true}},
	} {
		for _, live := range setting.live {
			t.Run(fmt.Sprintf("%s.%s/live=%t", setting.table, setting.field, live), func(t *testing.T) {
				assert, require := assert.New(t), require.New(t)
				f := newLocalActivationFixture(t, "")
				f.start(t)
				if live {
					require.NoError(f.controller.Enable(f.ctx, func(api.LocalEmbeddingsStatus) {}))
				}
				value := t.TempDir()
				if setting.table == "vector" {
					value = filepath.Join(value, "replacement-vectors.db")
					_, _, err := testutil.CreateIndexedSourceMessage(t.Context(), f.store, "archive@example.test", "later", "Later orchard", "Another orchard message.")
					require.NoError(err)
				} else {
					value = filepath.Join(value, "other-archive.db")
				}
				snapshot, err := config.ReadConfigFile(f.cfg.ConfigFilePath())
				require.NoError(err)
				written, err := config.EditConfigTables(f.cfg.ConfigFilePath(), snapshot.ETag, []config.TableEdit{{Path: []string{setting.table}, Values: map[string]any{setting.field: value}}})
				require.NoError(err)
				installs, builds := f.installs, len(f.runner.calls)
				err = f.controller.Enable(f.ctx, func(api.LocalEmbeddingsStatus) {})
				require.ErrorContains(err, "Restart msgvault")
				assert.Equal(installs, f.installs, "a pending storage change must be refused before installation")
				assert.Len(f.runner.calls, builds, "the builder must keep the serving archive")
				after, err := config.ReadConfigFile(f.cfg.ConfigFilePath())
				require.NoError(err)
				assert.Equal(written.Content, after.Content, "the refused activation must leave the pending settings unchanged")
				if setting.table == "vector" {
					results, _, err := f.handle.vf.HybridEngine.Search(f.ctx, hybrid.SearchRequest{Mode: hybrid.ModeVector, FreeText: "orchard", Limit: 10})
					require.NoError(err)
					assert.Len(results, 1)
				}
			})
		}
	}
}
