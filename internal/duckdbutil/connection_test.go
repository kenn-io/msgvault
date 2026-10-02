package duckdbutil

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuilderPolicyDefaults(t *testing.T) {
	assertions := assert.New(t)
	tempDir := filepath.Join(t.TempDir(), "spill")

	policy := BuilderPolicy(tempDir)

	assertions.Equal("2GB", policy.MemoryLimit)
	assertions.Equal(min(runtime.GOMAXPROCS(0), 2), policy.Threads)
	assertions.Equal(tempDir, policy.TempDirectory)
	assertions.Equal("32GB", policy.MaxTempDirectorySize)
}

func TestBuilderPolicyWithOverrides(t *testing.T) {
	assertions := assert.New(t)
	tempDir := filepath.Join(t.TempDir(), "spill")
	policy := BuilderPolicyWithOverrides(tempDir, BuilderOverrides{
		MemoryLimit:          "1536mIb",
		Threads:              3,
		MaxTempDirectorySize: "12gB",
	})

	assertions.Equal("1536mIb", policy.MemoryLimit)
	assertions.Equal(3, policy.Threads)
	assertions.Equal(tempDir, policy.TempDirectory)
	assertions.Equal("12gB", policy.MaxTempDirectorySize)

	defaultThreads := BuilderPolicyWithOverrides(tempDir, BuilderOverrides{Threads: 0})
	assertions.Equal(min(runtime.GOMAXPROCS(0), 2), defaultThreads.Threads)
}

func TestPolicyAppliesEffectiveSettings(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	tempDir := filepath.Join(t.TempDir(), "spill")
	policy := Policy{
		MemoryLimit:          "512MB",
		Threads:              3,
		TempDirectory:        tempDir,
		MaxTempDirectorySize: "2GB",
	}
	db, err := Open(context.Background(), policy)
	require.NoError(err)
	t.Cleanup(func() {
		require.NoError(db.Close())
	})

	require.NoError(db.Ping())
	assert.DirExists(tempDir)
	var threads int
	var preserveOrder bool
	var spillDir string
	require.NoError(db.QueryRow(`SELECT current_setting('threads'),
		current_setting('preserve_insertion_order'), current_setting('temp_directory')`).
		Scan(&threads, &preserveOrder, &spillDir))
	assert.Equal(policy.Threads, threads)
	assert.False(preserveOrder, "exports must be able to stream unordered results")
	assert.Equal(tempDir, spillDir)
}

func TestPolicyRejectsIncompleteOrUnsafeSettings(t *testing.T) {
	valid := Policy{
		MemoryLimit:          "512MB",
		Threads:              1,
		TempDirectory:        filepath.Join(t.TempDir(), "spill"),
		MaxTempDirectorySize: "2GB",
	}
	tests := []struct {
		name   string
		mutate func(*Policy)
	}{
		{name: "zero threads", mutate: func(policy *Policy) { policy.Threads = 0 }},
		{name: "empty memory limit", mutate: func(policy *Policy) { policy.MemoryLimit = "" }},
		{name: "relative temp directory", mutate: func(policy *Policy) { policy.TempDirectory = "spill" }},
		{name: "empty spill limit", mutate: func(policy *Policy) { policy.MaxTempDirectorySize = "" }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := valid
			test.mutate(&policy)
			db, err := Open(context.Background(), policy)
			if db != nil {
				require.NoError(t, db.Close())
			}
			require.Error(t, err)
		})
	}
}
