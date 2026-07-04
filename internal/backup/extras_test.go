package backup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/pack"
)

func TestCaptureExtrasDeletionsAndConfig(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)

	dataDir := t.TempDir()
	require.NoError(os.MkdirAll(filepath.Join(dataDir, "deletions", "sub"), 0o700))
	require.NoError(os.WriteFile(filepath.Join(dataDir, "deletions", "a.json"), []byte(`{"a":1}`), 0o600))
	require.NoError(os.WriteFile(filepath.Join(dataDir, "deletions", "sub", "b.json"), []byte(`{"b":2}`), 0o600))
	cfgPath := filepath.Join(dataDir, "config.toml")
	require.NoError(os.WriteFile(cfgPath, []byte("x = 1\n"), 0o600))

	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil)
	treeID, hasTree, err := CaptureExtras(ExtrasOptions{
		DataDir: dataDir, ConfigPath: cfgPath, IncludeConfig: true, AllowPlaintextSecrets: true,
	}, appender)
	require.NoError(err)
	require.True(hasTree)

	packs, entries, err := appender.Finish()
	require.NoError(err)
	require.NotEmpty(packs)
	known := map[pack.BlobID]IndexEntry{}
	for _, e := range entries {
		known[e.Blob] = e
	}
	treeData, err := r.ReadBlob(known, treeID, nil)
	require.NoError(err)
	var tree ExtrasTree
	require.NoError(json.Unmarshal(treeData, &tree))
	require.Len(tree.Entries, 3)
	assert.Equal("config.toml", tree.Entries[0].Path)
	assert.Equal("deletions/a.json", tree.Entries[1].Path)
	assert.Equal("deletions/sub/b.json", tree.Entries[2].Path)
	for _, e := range tree.Entries {
		blob, parseErr := pack.ParseBlobID(e.Blob)
		require.NoError(parseErr)
		content, readErr := r.ReadBlob(known, blob, nil)
		require.NoError(readErr)
		assert.Equal(e.Size, int64(len(content)), e.Path)
	}
}

func TestCaptureExtrasEmpty(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil)
	_, hasTree, err := CaptureExtras(ExtrasOptions{DataDir: t.TempDir()}, appender)
	require.NoError(err)
	require.False(hasTree)
}

func TestCaptureExtrasTokensGuard(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	dataDir := t.TempDir()
	require.NoError(os.MkdirAll(filepath.Join(dataDir, "tokens"), 0o700))
	require.NoError(os.WriteFile(filepath.Join(dataDir, "tokens", "t.json"), []byte("{}"), 0o600))
	require.NoError(os.WriteFile(filepath.Join(dataDir, "client_secret_web.json"), []byte("{}"), 0o600))

	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil)
	_, _, err := CaptureExtras(ExtrasOptions{DataDir: dataDir, IncludeTokens: true}, appender)
	require.ErrorContains(err, "encrypted repository")
	require.ErrorContains(err, "--include-tokens")

	// --include-config is just as sensitive: config.toml carries API keys, so
	// it fires the same guard and names the flag it tripped on.
	cfgPath := filepath.Join(dataDir, "config.toml")
	require.NoError(os.WriteFile(cfgPath, []byte("[server]\napi_key = \"secret\"\n"), 0o600))
	_, _, err = CaptureExtras(ExtrasOptions{DataDir: dataDir, ConfigPath: cfgPath, IncludeConfig: true}, appender)
	require.ErrorContains(err, "encrypted repository")
	require.ErrorContains(err, "--include-config")

	_, _, err = CaptureExtras(ExtrasOptions{
		DataDir: dataDir, ConfigPath: cfgPath, IncludeConfig: true, AllowPlaintextSecrets: true,
	}, appender)
	require.NoError(err)

	treeID, hasTree, err := CaptureExtras(ExtrasOptions{
		DataDir: dataDir, IncludeTokens: true, AllowPlaintextSecrets: true,
	}, appender)
	require.NoError(err)
	require.True(hasTree)

	_, entries, err := appender.Finish()
	require.NoError(err)
	known := map[pack.BlobID]IndexEntry{}
	for _, e := range entries {
		known[e.Blob] = e
	}
	treeData, err := r.ReadBlob(known, treeID, nil)
	require.NoError(err)
	var tree ExtrasTree
	require.NoError(json.Unmarshal(treeData, &tree))
	var paths []string
	for _, e := range tree.Entries {
		paths = append(paths, e.Path)
	}
	require.Equal([]string{"client_secret_web.json", "tokens/t.json"}, paths)
}

func TestCaptureExtrasRejectsSymlinks(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)

	dataDir := t.TempDir()
	outsideDir := t.TempDir()

	// Create a target file outside the data dir
	targetPath := filepath.Join(outsideDir, "sensitive.txt")
	require.NoError(os.WriteFile(targetPath, []byte("secret content"), 0o600))

	// Create a symlink inside deletions pointing to the outside file
	deletionsDir := filepath.Join(dataDir, "deletions")
	require.NoError(os.MkdirAll(deletionsDir, 0o700))
	symlinkPath := filepath.Join(deletionsDir, "link.txt")
	err := os.Symlink(targetPath, symlinkPath)
	if err != nil {
		// Skip if symlinks are not supported on this platform
		t.Skip("symlinks not supported on this platform")
	}

	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil)
	_, _, err = CaptureExtras(ExtrasOptions{DataDir: dataDir}, appender)
	require.Error(err)
	assert.Contains(err.Error(), "deletions/link.txt")
	assert.Contains(err.Error(), "not a regular file")

	// Verify the symlink target content is not embedded
	_, entries, errFinish := appender.Finish()
	require.NoError(errFinish)
	known := map[pack.BlobID]IndexEntry{}
	for _, e := range entries {
		known[e.Blob] = e
	}
	for _, e := range entries {
		content, _ := r.ReadBlob(known, e.Blob, nil)
		assert.NotContains(string(content), "secret content")
	}
}

// TestCaptureExtrasRejectsGlobbedSymlinks pins the fix extending the walk's
// symlink rejection to filepath.Glob's client_secret*.json results, which
// never went through addDir's filepath.WalkDir callback and so bypassed the
// os.ReadFile-follows-symlinks hazard TestCaptureExtrasRejectsSymlinks
// covers for the deletions/tokens walk.
func TestCaptureExtrasRejectsGlobbedSymlinks(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)

	dataDir := t.TempDir()
	outsideDir := t.TempDir()

	targetPath := filepath.Join(outsideDir, "sensitive.txt")
	require.NoError(os.WriteFile(targetPath, []byte("secret content"), 0o600))

	symlinkPath := filepath.Join(dataDir, "client_secret_evil.json")
	err := os.Symlink(targetPath, symlinkPath)
	if err != nil {
		t.Skip("symlinks not supported on this platform")
	}

	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil)
	defer appender.Abort()
	_, _, err = CaptureExtras(ExtrasOptions{
		DataDir: dataDir, IncludeTokens: true, AllowPlaintextSecrets: true,
	}, appender)
	require.Error(err)
	assert.Contains(err.Error(), "client_secret_evil.json")
	assert.Contains(err.Error(), "not a regular file")

	// Verify the symlink target content is not embedded.
	_, entries, errFinish := appender.Finish()
	require.NoError(errFinish)
	known := map[pack.BlobID]IndexEntry{}
	for _, e := range entries {
		known[e.Blob] = e
	}
	for _, e := range entries {
		content, _ := r.ReadBlob(known, e.Blob, nil)
		assert.NotContains(string(content), "secret content")
	}
}
