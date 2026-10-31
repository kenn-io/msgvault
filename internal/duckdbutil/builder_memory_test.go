package duckdbutil

import (
	"fmt"
	"math"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuilderMemoryBudget(t *testing.T) {
	for _, tc := range []struct{ available, want uint64 }{
		{0, 256 << 20}, {1, 256 << 20}, {1 << 30, 256 << 20},
		{(1 << 30) - 1, 256 << 20}, {(1 << 30) + 4, (256 << 20) + 1},
		{8 << 30, 2 << 30}, {(64 << 30) - 4, (16 << 30) - 1},
		{64 << 30, 16 << 30}, {(64 << 30) + 4, 16 << 30},
		{192 << 30, 16 << 30}, {math.MaxUint64, 16 << 30},
	} {
		assert.Equal(t, tc.want, builderMemoryBudget(tc.available))
	}
}

func FuzzBuilderMemoryBudgetMonotonic(f *testing.F) {
	f.Add(uint64(0), uint64(math.MaxUint64))
	f.Add(uint64(1<<30), uint64(192<<30))
	f.Fuzz(func(t *testing.T, a, b uint64) {
		a, b = min(a, b), max(a, b)
		assert.LessOrEqual(t, builderMemoryBudget(a), builderMemoryBudget(b))
	})
}

func FuzzBuilderMemoryBudgetBounds(f *testing.F) {
	f.Add(uint64(0))
	f.Add(uint64(math.MaxUint64))
	f.Fuzz(func(t *testing.T, available uint64) {
		budget := builderMemoryBudget(available)
		assert.GreaterOrEqual(t, budget, uint64(256<<20))
		assert.LessOrEqual(t, budget, uint64(16<<30))
	})
}

func TestCgroupAvailableMemory(t *testing.T) {
	for _, tc := range []struct {
		name, root, membership, mount string
		files                         map[string]string
		want                          uint64
		wantErr                       bool
	}{
		{"ancestor", "/", "/parent/child", "/sys/fs/cgroup", map[string]string{
			"parent/child/memory.max": "8589934592", "parent/child/memory.high": "max", "parent/child/memory.current": "0",
			"parent/memory.max": "4294967296", "parent/memory.high": "3221225472", "parent/memory.current": "1073741824",
		}, 2 << 30, false},
		{"missing leaf retains ancestor", "/", "/parent/child", "/sys/fs/cgroup", map[string]string{
			"parent/memory.max": "536870912", "parent/memory.high": "max", "parent/memory.current": "134217728",
		}, 384 << 20, true},
		{"malformed leaf retains other bounds", "/", "/parent/child", "/sys/fs/cgroup", map[string]string{
			"parent/child/memory.max": "invalid", "parent/child/memory.high": "1073741824", "parent/child/memory.current": "0",
			"parent/memory.max": "536870912", "parent/memory.high": "max", "parent/memory.current": "134217728",
		}, 384 << 20, true},
		{"exhausted", "/", "/child", "/sys/fs/cgroup", map[string]string{
			"child/memory.max": "100", "child/memory.high": "max", "child/memory.current": "101",
		}, 0, false},
		{"unlimited", "/", "/", "/sys/fs/cgroup", map[string]string{
			"memory.max": "max", "memory.high": "max",
		}, 64 << 30, false},
		{"non-root mount", "/parent", "/parent/child", "/containers/memory", map[string]string{
			"child/memory.max": "8589934592", "child/memory.high": "max", "child/memory.current": "1073741824",
			"memory.max": "4294967296", "memory.high": "max", "memory.current": "1073741824",
		}, 3 << 30, false},
		{"escaped mount", "/", "/", "/memory\\040group", map[string]string{
			"memory.max": "4294967296", "memory.high": "max", "memory.current": "1073741824",
		}, 3 << 30, false},
		{"malformed", "/", "/child", "/sys/fs/cgroup", map[string]string{
			"child/memory.max": "invalid", "child/memory.high": "max",
		}, 64 << 30, true},
		{"missing ancestor preserves bound", "/", "/parent/child", "/sys/fs/cgroup", map[string]string{
			"parent/child/memory.max": "1073741824", "parent/child/memory.high": "max", "parent/child/memory.current": "536870912",
		}, 512 << 20, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys := fstest.MapFS{
				"proc/self/cgroup": {Data: []byte("0::" + tc.membership + "\n")},
				"proc/self/mountinfo": {Data: []byte(fmt.Sprintf(
					"1 0 0:1 %s %s rw - cgroup2 cgroup rw\n", tc.root, tc.mount))},
			}
			mount := tc.mount
			if tc.name == "escaped mount" {
				mount = "/memory group"
			}
			for name, data := range tc.files {
				fsys[mount[1:]+"/"+name] = &fstest.MapFile{Data: []byte(data + "\n")}
			}
			got, err := cgroupAvailableMemory(fsys, 64<<30)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

func FuzzCgroupHeadroom(f *testing.F) {
	f.Add(uint64(100), uint64(101))
	f.Add(uint64(math.MaxUint64), uint64(0))
	f.Fuzz(func(t *testing.T, limit, used uint64) {
		fsys := fstest.MapFS{
			"proc/self/cgroup":      {Data: []byte("0::/\n")},
			"proc/self/mountinfo":   {Data: []byte("1 0 0:1 / /memory rw - cgroup2 cgroup rw\n")},
			"memory/memory.max":     {Data: fmt.Appendf(nil, "%d", limit)},
			"memory/memory.high":    {Data: []byte("max")},
			"memory/memory.current": {Data: fmt.Appendf(nil, "%d", used)},
		}
		got, err := cgroupAvailableMemory(fsys, math.MaxUint64)
		require.NoError(t, err)
		var want uint64
		if used < limit {
			want = limit - used
		}
		assert.Equal(t, want, got)
	})
}

func TestCgroupAvailableMemoryMountOrder(t *testing.T) {
	wide := "1 0 0:1 / /memory rw - cgroup2 cgroup rw\n"
	narrow := "2 0 0:1 /parent/child /subtree rw - cgroup2 cgroup rw\n"
	for _, mounts := range []string{wide + narrow, narrow + wide} {
		fsys := fstest.MapFS{
			"proc/self/cgroup":                {Data: []byte("0::/parent/child\n")},
			"proc/self/mountinfo":             {Data: []byte(mounts)},
			"subtree/memory.max":              {Data: []byte("max")},
			"subtree/memory.high":             {Data: []byte("max")},
			"memory/parent/child/memory.max":  {Data: []byte("max")},
			"memory/parent/child/memory.high": {Data: []byte("max")},
			"memory/parent/memory.max":        {Data: []byte("536870912")},
			"memory/parent/memory.high":       {Data: []byte("max")},
			"memory/parent/memory.current":    {Data: []byte("134217728")},
		}
		got, err := cgroupAvailableMemory(fsys, 64<<30)
		require.NoError(t, err)
		assert.Equal(t, uint64(384<<20), got)
	}
}
