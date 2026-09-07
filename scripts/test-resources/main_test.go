package main

import (
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShardBudget(t *testing.T) {
	for _, tc := range []struct {
		name string
		cpus int
		gib  uint64
		want int
	}{
		{"small CPU budget", 8, 256, 0},
		{"small memory budget", 128, 32, 0},
		{"unknown memory", 128, 0, 0},
		{"CPU limited", 32, 256, 3},
		{"memory limited", 128, 64, 7},
		{"large budget", 128, 256, 15},
		{"cap very large budgets", 512, 1024, 16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, shardBudget(resources{tc.cpus, tc.gib << 30}, 4))
		})
	}
}

func TestCgroupAncestorLimits(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	files := fstest.MapFS{
		"parent/child/cpu.max":        {Data: []byte("max 100000\n")},
		"parent/child/memory.max":     {Data: []byte("max\n")},
		"parent/child/memory.high":    {Data: []byte("max\n")},
		"parent/child/memory.current": {Data: []byte("0\n")},
		"parent/cpu.max":              {Data: []byte("250000 100000\n")},
		"parent/memory.max":           {Data: []byte("34359738368\n")}, // 32 GiB
		"parent/memory.high":          {Data: []byte("17179869184\n")}, // 16 GiB
		"parent/memory.current":       {Data: []byte("4294967296\n")},  // 4 GiB
	}
	got, err := limitCgroup(files, "parent/child", resources{128, 256 << 30})
	require.NoError(err)
	assert.Equal(resources{2, 12 << 30}, got)
	assert.Zero(shardBudget(got, 4))

	// A soft memory limit can already be exceeded; subtraction must not wrap.
	files["parent/memory.current"].Data = []byte("21474836480\n")
	got, err = limitCgroup(files, "parent/child", resources{128, 256 << 30})
	require.NoError(err)
	assert.Zero(got.available)
}

func TestAggregateBudgetIncludesRemainder(t *testing.T) {
	assert := assert.New(t)
	for _, r := range []resources{{32, 64 << 30}, {64, 64 << 30}, {128, 256 << 30}, {512, 1024 << 30}} {
		for packages := 1; packages <= 16; packages++ {
			shards := shardBudget(r, packages)
			if shards == 0 {
				continue
			}
			processes := packages*shards + remainderParallel
			assert.LessOrEqual(processes*processProcs, r.cpus, "CPU budget, %d groups", packages)
			assert.LessOrEqual(uint64(processes)*processMemory, r.available, "memory allowance, %d groups", packages)
		}
	}
}

func TestCgroupRootLimits(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// A cgroup namespace may expose its container's limits at the mount root.
	files := fstest.MapFS{
		"cpu.max":        {Data: []byte("6400000 100000\n")},
		"memory.max":     {Data: []byte("137438953472\n")},
		"memory.high":    {Data: []byte("max\n")},
		"memory.current": {Data: []byte("68719476736\n")},
	}
	got, err := limitCgroup(files, ".", resources{128, 256 << 30})
	require.NoError(err)
	assert.Equal(resources{64, 64 << 30}, got)
	assert.Equal(7, shardBudget(got, 4))
	files["cpu.max"].Data = []byte("bad quota")
	_, err = limitCgroup(files, ".", resources{128, 256 << 30})
	require.Error(err)
}

func TestCgroupMissingOrInvalidPath(t *testing.T) {
	for _, group := range []string{"missing", "../outside"} {
		_, err := limitCgroup(fstest.MapFS{}, group, resources{128, 256 << 30})
		require.Error(t, err)
	}
}
