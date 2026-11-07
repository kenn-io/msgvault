package agentguide

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManagedBlock(t *testing.T) {
	block := ManagedBlock()
	assert.LessOrEqual(t, len(block), 2000)
	assert.True(t, strings.HasPrefix(block, BlockBegin+"\n"))
	assert.True(t, strings.HasSuffix(block, "\n"+BlockEnd))
}

func TestUpsertManagedBlock(t *testing.T) {
	for _, content := range []string{"", "# Project", "# Project\n", "# Project\n\n", "# Project\r\n"} {
		t.Run(strings.ReplaceAll(content, "\n", "LF"), func(t *testing.T) {
			updated, err := UpsertManagedBlock(content)
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(updated, content), "preserve every pre-existing byte")
			assert.Contains(t, updated, ManagedBlock())
			repeated, err := UpsertManagedBlock(updated)
			require.NoError(t, err)
			assert.Equal(t, updated, repeated)
		})
	}
	for _, newline := range []string{"\n", "\r\n"} {
		prefix, suffix := "# Other instructions"+newline+newline, newline+"Keep this."
		updated, err := UpsertManagedBlock(prefix + BlockBegin + newline + "stale" + newline + BlockEnd + suffix)
		require.NoError(t, err)
		assert.Equal(t, prefix+ManagedBlock()+suffix, updated)
	}
}

func TestUpsertRejectsAmbiguousMarkers(t *testing.T) {
	for name, content := range map[string]string{
		"orphan begin":  BlockBegin,
		"orphan end":    BlockEnd,
		"reversed":      BlockEnd + "\n" + BlockBegin,
		"duplicate":     ManagedBlock() + "\n" + ManagedBlock(),
		"inline begin":  "keep " + ManagedBlock(),
		"inline end":    ManagedBlock() + " keep",
		"damaged begin": "<!-- BEGIN MSGVAULT MCP --\n" + BlockEnd,
		"damaged end":   BlockBegin + "\n<!-- END MSGVAULT MCP --",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := UpsertManagedBlock(content)
			require.Error(t, err)
		})
	}
}

func FuzzManagedBlockPreservesOutside(f *testing.F) {
	f.Add("# Project\n", "\nKeep this.")
	f.Add("\r\n", "\r\n")
	f.Add("", "")
	f.Fuzz(func(t *testing.T, before, after string) {
		// Marker-containing input is the conflict domain covered separately.
		if strings.Contains(before+after, "<!-- BEGIN MSGVAULT MCP") || strings.Contains(before+after, "<!-- END MSGVAULT MCP") {
			t.Skip()
		}
		prefix, suffix := before+"\n", "\n"+after
		updated, err := UpsertManagedBlock(prefix + BlockBegin + "\nstale\n" + BlockEnd + suffix)
		require.NoError(t, err)
		assert.Equal(t, prefix+ManagedBlock()+suffix, updated)
		repeated, err := UpsertManagedBlock(updated)
		require.NoError(t, err)
		assert.Equal(t, updated, repeated)
	})
}

func TestWriteInstructions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	changed, err := WriteInstructions(path)
	require.NoError(t, err)
	assert.True(t, changed)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, ManagedBlock()+"\n", string(content))

	initial := "# Project\r\n\r\n" + BlockBegin + "\r\nstale\r\n" + BlockEnd + "\r\nKeep this."
	require.NoError(t, os.WriteFile(path, []byte(initial), 0o600))
	require.NoError(t, os.Chmod(path, 0o640))
	changed, err = WriteInstructions(path)
	require.NoError(t, err)
	assert.True(t, changed)
	content, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "# Project\r\n\r\n"+ManagedBlock()+"\r\nKeep this.", string(content))
	info, err := os.Stat(path)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())
	}
	oldTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(path, oldTime, oldTime))
	changed, err = WriteInstructions(path)
	require.NoError(t, err)
	assert.False(t, changed)
	info, err = os.Stat(path)
	require.NoError(t, err)
	assert.True(t, oldTime.Equal(info.ModTime()), "an idempotent update must not rewrite the file")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "temporary replacement files must be removed")
}

func TestWriteInstructionsRejectsUnsafeTargets(t *testing.T) {
	for _, kind := range []string{"symlink", "dangling symlink", "directory", "read-only", "malformed", "missing parent"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "AGENTS.md")
			original := "Keep exactly this."
			target := filepath.Join(dir, "target.md")
			require.NoError(t, os.WriteFile(target, []byte(original), 0o600))
			switch kind {
			case "symlink", "dangling symlink":
				if kind == "dangling symlink" {
					target += ".missing"
				}
				if err := os.Symlink(target, path); err != nil {
					t.Skipf("symlink creation unavailable: %v", err)
				}
			case "directory":
				require.NoError(t, os.Mkdir(path, 0o700))
			case "read-only":
				if runtime.GOOS == "windows" {
					t.Skip("POSIX file mode check")
				}
				require.NoError(t, os.WriteFile(path, []byte(original), 0o400))
			case "malformed":
				original = BlockBegin
				require.NoError(t, os.WriteFile(path, []byte(original), 0o600))
			case "missing parent":
				path = filepath.Join(dir, "missing", "AGENTS.md")
			}
			changed, err := WriteInstructions(path)
			require.Error(t, err)
			assert.False(t, changed)
			if kind == "malformed" || kind == "read-only" {
				content, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, original, string(content))
			}
			content, err := os.ReadFile(filepath.Join(dir, "target.md"))
			require.NoError(t, err)
			assert.Equal(t, "Keep exactly this.", string(content))
		})
	}
}
