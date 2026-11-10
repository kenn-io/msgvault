package agentdemo_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/fakevault/agentdemo"
)

func TestGenerateDirRoundTripsManifest(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	m, err := agentdemo.GenerateDir(t.Context(), agentdemo.Options{Dir: dir, Seed: 1})
	require.NoError(err)
	loaded, err := agentdemo.Load(dir)
	require.NoError(err)
	assert.Equal(m, loaded)
	assert.FileExists(filepath.Join(dir, "msgvault.db"))
}

func TestGenerateDirRefusesExistingDatabase(t *testing.T) {
	for _, name := range []string{"regular file", "dangling symlink"} {
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			dir := t.TempDir()
			path := filepath.Join(dir, "msgvault.db")
			target := filepath.Join(t.TempDir(), "untouched.db")
			if name == "regular file" {
				require.NoError(os.WriteFile(path, []byte("keep me"), 0600))
			} else {
				require.NoError(os.Symlink(target, path))
			}
			_, err := agentdemo.GenerateDir(t.Context(), agentdemo.Options{Dir: dir})
			require.ErrorContains(err, "already exists")
			if name == "regular file" {
				data, err := os.ReadFile(path)
				require.NoError(err)
				assert.Equal(t, "keep me", string(data))
			} else {
				assert.NoFileExists(t, target)
			}
		})
	}
}

func TestGenerateRejectsUnsupportedAsOf(t *testing.T) {
	for _, asOf := range []time.Time{
		time.Date(1989, 12, 31, 23, 59, 59, 0, time.UTC),
		// The clock itself is plausible, but the oldest background mail is not.
		time.Date(1990, 2, 1, 0, 0, 0, 0, time.UTC),
		time.Now().AddDate(1, 0, 0),
	} {
		t.Run(asOf.Format(time.RFC3339), func(t *testing.T) {
			for _, entry := range []string{"GenerateDir", "Generate"} {
				t.Run(entry, func(t *testing.T) {
					dir := filepath.Join(t.TempDir(), "output")
					opts := agentdemo.Options{Dir: dir, AsOf: asOf}
					var err error
					if entry == "GenerateDir" {
						_, err = agentdemo.GenerateDir(t.Context(), opts)
					} else {
						// Invalid clocks must return before opening the store or creating blobs.
						_, err = agentdemo.Generate(t.Context(), nil, dir, opts)
					}
					require.ErrorContains(t, err, "--as-of")
					assert.NoDirExists(t, dir)
				})
			}
		})
	}
}
