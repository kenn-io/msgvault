package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUserFacingCommandsDoNotImportInternalRemote(t *testing.T) {
	root := repoRootForImportBoundaryTest(t)
	files, err := filepath.Glob(filepath.Join(root, "cmd/msgvault/cmd/*.go"))
	require.NoError(t, err, "glob command files")
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		data, err := os.ReadFile(file)
		require.NoError(t, err, "read %s", file)
		assert.NotContains(t, string(data), `"go.kenn.io/msgvault/internal/remote"`, filepath.Base(file))
	}
}

func repoRootForImportBoundaryTest(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime caller")

	dir := filepath.Dir(currentFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "repository root containing go.mod")
		dir = parent
	}
}
