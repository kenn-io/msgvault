package duckdbutil

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The interactive defaults are laptop-sized; a large archive must be able to
// raise them, because a query that spills past max_temp_directory_size fails
// outright ("Out of Memory Error") instead of running slowly.
func TestInteractivePolicyWithOverrides(t *testing.T) {
	assertions := assert.New(t)
	base := InteractivePolicy("/tmp/spill")
	assertions.Equal("512MB", base.MemoryLimit)
	assertions.Equal("2GB", base.MaxTempDirectorySize)

	got := InteractivePolicyWithOverrides("/tmp/spill", InteractiveOverrides{
		MemoryLimit:          "8GB",
		Threads:              8,
		MaxTempDirectorySize: "40GB",
	})
	assertions.Equal("8GB", got.MemoryLimit)
	assertions.Equal(8, got.Threads)
	assertions.Equal("40GB", got.MaxTempDirectorySize)
	assertions.Equal("/tmp/spill", got.TempDirectory)

	// Zero values leave the defaults intact.
	unchanged := InteractivePolicyWithOverrides("/tmp/spill", InteractiveOverrides{})
	assertions.Equal(base, unchanged)
}
