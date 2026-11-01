package testutil

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validRelativePaths returns a fresh slice of relative paths that should pass
// validation and be writable. Used by TestValidateRelativePath and
// TestWriteFileWithValidPaths.
func validRelativePaths() []string {
	paths := []string{
		"simple.txt",
		"subdir/file.txt",
		"a/b/c/deep.txt",
		"file-with-dots.test.txt",
		"./current.txt",
		// Paths that look like ".." but are actually valid filenames
		"..foo",           // starts with dots but is a valid filename
		"subdir/..hidden", // hidden-style name in subdir
	}
	// "…." (four dots) is valid on Unix but Windows strips trailing dots,
	// treating it like ".." which escapes the directory.
	if runtime.GOOS != "windows" {
		paths = append(paths, "....") // four dots - valid filename, not parent escape
	}
	return paths
}

func TestValidateRelativePath(t *testing.T) {
	dir := t.TempDir()

	// Invalid paths from shared fixture
	for _, tt := range PathTraversalCases() {
		t.Run(tt.Name, func(t *testing.T) {
			err := validateRelativePath(dir, tt.Path)
			assert.Error(t, err, "validateRelativePath(%q) expected error", tt.Path)
		})
	}

	// Valid paths from shared fixture
	for _, path := range validRelativePaths() {
		t.Run("valid "+path, func(t *testing.T) {
			err := validateRelativePath(dir, path)
			assert.NoError(t, err, "validateRelativePath(%q)", path)
		})
	}
}

func TestPathTraversalCasesReturnsFreshSlice(t *testing.T) {
	a := PathTraversalCases()
	b := PathTraversalCases()

	// Mutate the first slice and verify the second is unaffected.
	require.NotEmpty(t, a, "PathTraversalCases() returned empty slice")
	require.NotEmpty(t, b, "PathTraversalCases() returned empty slice on second call")
	original := b[0].Name
	a[0].Name = "MUTATED"
	assert.Equal(t, original, b[0].Name, "PathTraversalCases() returned shared slice: mutating one affected the other")
}

func TestWriteFileWithValidPaths(t *testing.T) {
	dir := t.TempDir()

	for _, name := range validRelativePaths() {
		t.Run(name, func(t *testing.T) {
			WriteAndVerifyFile(t, dir, name, []byte("data"))
		})
	}
}
