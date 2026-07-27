package duckdbutil

import (
	"context"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPolicyAppliesEffectiveSettings(t *testing.T) {
	tempDir := filepath.Join(t.TempDir(), "spill")
	policy := Policy{
		MemoryLimit:          "512MB",
		Threads:              3,
		TempDirectory:        tempDir,
		MaxTempDirectorySize: "2GB",
	}
	db, err := Open(context.Background(), policy)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	var threads int
	var memory, configuredTemp, spill string
	var preserveInsertionOrder bool
	err = db.QueryRow(`
		SELECT current_setting('threads')::INTEGER,
		       current_setting('memory_limit'),
		       current_setting('temp_directory'),
		       current_setting('max_temp_directory_size'),
		       current_setting('preserve_insertion_order')::BOOLEAN
	`).Scan(&threads, &memory, &configuredTemp, &spill, &preserveInsertionOrder)
	require.NoError(t, err)

	assert.Equal(t, 3, threads)
	assert.InDelta(t, 512_000_000, parseDuckDBBytes(t, memory), 1_048_576)
	assert.Equal(t, filepath.Clean(tempDir), filepath.Clean(configuredTemp))
	// DuckDB renders GiB settings with one decimal place, so parsing the
	// displayed value loses up to 0.05 GiB of precision.
	assert.InDelta(t, 2_000_000_000, parseDuckDBBytes(t, spill), 1<<27)
	assert.False(t, preserveInsertionOrder)
	assert.DirExists(t, tempDir)
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

func TestPolicyConstructorsCapThreads(t *testing.T) {
	interactive := InteractivePolicy(filepath.Join(t.TempDir(), "interactive"))
	builder := BuilderPolicy(filepath.Join(t.TempDir(), "builder"))

	assert.Equal(t, min(runtime.GOMAXPROCS(0), 4), interactive.Threads)
	assert.Equal(t, "512MB", interactive.MemoryLimit)
	assert.Equal(t, "2GB", interactive.MaxTempDirectorySize)
	assert.Equal(t, min(runtime.GOMAXPROCS(0), 8), builder.Threads)
	assert.Equal(t, "2GB", builder.MemoryLimit)
	assert.Equal(t, "8GB", builder.MaxTempDirectorySize)
}

func parseDuckDBBytes(t *testing.T, value string) int64 {
	t.Helper()
	parts := strings.Fields(value)
	require.Len(t, parts, 2)
	number, err := strconv.ParseFloat(parts[0], 64)
	require.NoError(t, err)
	multipliers := map[string]float64{
		"bytes": 1,
		"KiB":   1 << 10,
		"MiB":   1 << 20,
		"GiB":   1 << 30,
		"TiB":   1 << 40,
	}
	multiplier, ok := multipliers[parts[1]]
	require.True(t, ok, "unknown DuckDB byte unit %q", parts[1])
	return int64(number * multiplier)
}
