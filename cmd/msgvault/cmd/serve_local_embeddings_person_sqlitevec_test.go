//go:build sqlite_vec

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/vector"
)

func TestLocalEmbeddingsBuildRechecksLivePersonAuthorization(t *testing.T) {
	for _, change := range []string{"allow", "disable", "remove custom config", "revoke consent"} {
		t.Run(change, func(t *testing.T) {
			must := require.New(t)
			checks := assert.New(t)
			f := newLocalActivationFixture(t, "nomic-embed-text", "\n[vector.people]\nenabled = true\nretention_posture = \"zero_data_retention\"\ntraining_posture = \"no_training\"\n")
			if change == "remove custom config" {
				path := filepath.Join(f.cfg.HomeDir, "custom.toml")
				must.NoError(os.Rename(f.cfg.ConfigFilePath(), path))
				var err error
				f.cfg, err = config.Load(path, f.cfg.HomeDir)
				must.NoError(err)
				f.state.cfg = f.cfg
			}
			f.cfg.Vector.Enabled = false
			f.cfg.Vector.Embeddings.BatchSize = 1
			must.NoError(f.cfg.Save())
			for _, address := range []string{"one@example.test", "two@example.test"} {
				participant, err := f.store.EnsureParticipantByIdentifier("email", address, "Curated Live Person "+address)
				must.NoError(err)
				_, _, err = f.store.CreatePersonFromParticipantContext(f.ctx, participant)
				must.NoError(err)
			}
			profile, err := f.cfg.Vector.SemanticPersonEmbeddingProfile()
			must.NoError(err)
			_, err = f.store.EnsurePersonSemanticEmbeddingProfile(f.ctx, profile)
			must.NoError(err)
			_, _, err = f.store.GrantPersonSemanticEmbeddingConsent(f.ctx, profile.Fingerprint, "test")
			must.NoError(err)
			f.start(t)
			var personRequests, messageRequests atomic.Int32
			f.onEmbedding = func(inputs []string) error {
				if !strings.Contains(inputs[0], "Curated Live Person") {
					messageRequests.Add(1)
					return nil
				}
				if personRequests.Add(1) != 1 {
					return nil
				}
				switch change {
				case "disable":
					snapshot, err := config.ReadConfigFile(f.cfg.ConfigFilePath())
					if err != nil {
						return err
					}
					_, err = config.EditConfigTables(f.cfg.ConfigFilePath(), snapshot.ETag, []config.TableEdit{{Path: []string{"vector", "people"}, Values: map[string]any{"enabled": false}}})
					return err
				case "remove custom config":
					return os.Remove(f.cfg.ConfigFilePath())
				case "revoke consent":
					_, err := f.store.RevokePersonSemanticEmbeddingConsent(f.ctx, profile.Fingerprint, "test")
					return err
				}
				return nil
			}
			must.NoError(f.controller.Enable(f.ctx, false, func(api.LocalEmbeddingsStatus) {}))
			checks.Equal(int32(1), messageRequests.Load(), "message indexing must finish")
			want := int32(1)
			if change == "allow" {
				want = 2
			}
			checks.Equal(want, personRequests.Load(), "the next batch must use live authorization")
			beforeQuery := personRequests.Load()
			results, err := f.handle.vf.PersonSearchEngine.Search(f.ctx, "Curated Live Person", 5)
			switch change {
			case "allow":
				must.NoError(err)
				checks.Len(results, 2)
			case "revoke consent":
				must.ErrorIs(err, vector.ErrSemanticPersonEmbeddingConsentRequired)
			default:
				must.ErrorIs(err, vector.ErrSemanticPersonEmbeddingsDisabled)
			}
			if change != "allow" {
				checks.Equal(beforeQuery, personRequests.Load(), "denied queries must not reach the provider")
			}
		})
	}
}
