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
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

func TestLocalEmbeddingsActivationKeepsBuilderStorage(t *testing.T) {
	for _, storage := range []string{"separate home and config", "saved path after activation"} {
		t.Run(storage, func(t *testing.T) {
			f := newLocalActivationFixture(t, "")
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
			}
			installs, builds := f.installs, len(f.runner.calls)
			err := f.controller.Enable(f.ctx, func(api.LocalEmbeddingsStatus) {})
			if storage == "saved path after activation" {
				require.ErrorContains(t, err, "Restart msgvault")
				assert.Equal(t, installs, f.installs)
				assert.Len(t, f.runner.calls, builds)
			} else {
				require.NoError(t, err)
			}
			results, _, err := f.handle.vf.HybridEngine.Search(f.ctx, hybrid.SearchRequest{Mode: hybrid.ModeVector, FreeText: "orchard", Limit: 10})
			require.NoError(t, err)
			assert.Len(t, results, 1)
		})
	}
}

func TestLocalEmbeddingsRefusesPendingArchiveStorageChange(t *testing.T) {
	for _, field := range []string{"data_dir", "database_url"} {
		for _, live := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/live=%t", field, live), func(t *testing.T) {
				assert, require := assert.New(t), require.New(t)
				f := newLocalActivationFixture(t, "")
				f.start(t)
				if live {
					require.NoError(f.controller.Enable(f.ctx, func(api.LocalEmbeddingsStatus) {}))
				}
				next := *f.cfg
				value := t.TempDir()
				if field == "data_dir" {
					next.Data.DataDir = value
				} else {
					value = filepath.Join(value, "other-archive.db")
					next.Data.DatabaseURL = value
				}
				other, err := store.Open(next.DatabaseDSN())
				require.NoError(err)
				t.Cleanup(func() { require.NoError(other.Close()) })
				require.NoError(other.InitSchema())
				_, _, err = testutil.CreateIndexedSourceMessage(t.Context(), other, "other@example.test", "other", "Other archive", "A different message archive.")
				require.NoError(err)
				snapshot, err := config.ReadConfigFile(f.cfg.ConfigFilePath())
				require.NoError(err)
				written, err := config.EditConfigTables(f.cfg.ConfigFilePath(), snapshot.ETag, []config.TableEdit{{Path: []string{"data"}, Values: map[string]any{field: value}}})
				require.NoError(err)
				installs, builds := f.installs, len(f.runner.calls)
				err = f.controller.Enable(f.ctx, func(api.LocalEmbeddingsStatus) {})
				assert.ErrorContains(err, "Restart msgvault")
				assert.Equal(installs, f.installs, "a pending storage change must be refused before installation")
				assert.Len(f.runner.calls, builds, "the builder must keep the serving archive")
				after, err := config.ReadConfigFile(f.cfg.ConfigFilePath())
				require.NoError(err)
				assert.Equal(written.Content, after.Content, "the refused activation must leave the pending settings unchanged")
			})
		}
	}
}
