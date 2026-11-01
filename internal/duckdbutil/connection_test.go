package duckdbutil

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuilderErrorPreservesCause(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	policy := BuilderPolicyWithOverrides(filepath.Join(t.TempDir(), "spill"), BuilderOverrides{
		MemoryLimit: "1MiB", Threads: 1, MaxTempDirectorySize: "8MB",
	})
	db, err := Open(t.Context(), policy)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(db.Close()) })
	_, oom := db.ExecContext(t.Context(), `CREATE TABLE too_large AS
		SELECT i, repeat('x', 10000) || i::VARCHAR AS payload FROM range(10000) t(i)`)
	requirements.Error(oom)
	requirements.True(IsOutOfMemory(oom))
	for _, cause := range []error{oom, errors.Join(context.Canceled, oom)} {
		got := BuilderError(cause, policy)
		requirements.ErrorIs(got, cause)
		assertions.Contains(got.Error(), "analytics.builder_memory_limit=1MiB")
		assertions.Contains(got.Error(), "analytics.builder_threads=1")
		assertions.Contains(got.Error(), "analytics.builder_temp_limit=8MB")
		assertions.Contains(got.Error(), "msgvault build-cache")
		assertions.Same(got, BuilderError(got, BuilderPolicy(t.TempDir())),
			"nested build paths must retain the actual failing connection's limits")
	}
	for _, cause := range []error{nil, context.Canceled, errors.New("unrelated error")} {
		assertions.Equal(cause, BuilderError(cause, policy))
	}
}

func TestBuilderPolicyDefaults(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	tempDir := filepath.Join(t.TempDir(), "spill")

	policy := BuilderPolicy(tempDir)

	requirements.True(ValidSize(policy.MemoryLimit))
	if policy.MemoryLimit != "2GB" {
		budget, err := strconv.ParseUint(strings.TrimSuffix(policy.MemoryLimit, "B"), 10, 64)
		requirements.NoError(err)
		assertions.GreaterOrEqual(budget, uint64(256<<20))
		assertions.LessOrEqual(budget, uint64(16<<30))
	}
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
	var threads, asyncThreads int
	var preserveOrder bool
	var spillDir string
	require.NoError(db.QueryRow(`SELECT current_setting('threads'), current_setting('async_threads'),
		current_setting('preserve_insertion_order'), current_setting('temp_directory')`).
		Scan(&threads, &asyncThreads, &preserveOrder, &spillDir))
	assert.Equal(policy.Threads, threads)
	assert.Equal(policy.Threads, asyncThreads)
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
