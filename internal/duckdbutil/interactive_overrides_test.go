package duckdbutil

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The interactive defaults are laptop-sized; a large archive must be able to
// raise them, because a query that spills past max_temp_directory_size fails
// outright ("Out of Memory Error") instead of running slowly.
func TestInteractivePolicyWithOverrides(t *testing.T) {
	base := InteractivePolicy("/tmp/spill")
	assert.Equal(t, "512MB", base.MemoryLimit)
	assert.Equal(t, "2GB", base.MaxTempDirectorySize)

	got := InteractivePolicyWithOverrides("/tmp/spill", InteractiveOverrides{
		MemoryLimit:          "8GB",
		Threads:              8,
		MaxTempDirectorySize: "40GB",
	})
	assert.Equal(t, "8GB", got.MemoryLimit)
	assert.Equal(t, 8, got.Threads)
	assert.Equal(t, "40GB", got.MaxTempDirectorySize)
	assert.Equal(t, "/tmp/spill", got.TempDirectory)

	// Zero values leave the defaults intact.
	unchanged := InteractivePolicyWithOverrides("/tmp/spill", InteractiveOverrides{})
	assert.Equal(t, base, unchanged)
}
