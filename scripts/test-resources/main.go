package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"

	"github.com/shirou/gopsutil/v4/mem"
)

type resources struct {
	cpus      int
	available uint64
}

const (
	processProcs      = 2
	remainderParallel = 4
	processMemory     = 2 << 30
)

// Zero selects the existing sequential package schedule. Wider execution needs
// both CPU and memory headroom, with an upper bound even on very large hosts.
func shardBudget(r resources, packages int) int {
	if r.cpus < 32 || r.available < 64<<30 || packages < 1 {
		return 0
	}
	// Reserve the remainder first, then divide the remaining process slots
	// across all concurrent shard groups. Memory is a planning allowance, not
	// an enforced per-process limit (in particular for native allocations).
	cpuShards := (r.cpus/processProcs - remainderParallel) / packages
	memoryShards := (r.available/processMemory - remainderParallel) / uint64(packages)
	return min(16, cpuShards, int(min(uint64(16), memoryShards)))
}

func limitCgroup(fsys fs.FS, group string, r resources) (resources, error) {
	if !fs.ValidPath(group) {
		return resources{}, errors.New("invalid cgroup path")
	}
	for {
		cpu, err := fs.ReadFile(fsys, path.Join(group, "cpu.max"))
		if err != nil && (group != "." || !errors.Is(err, fs.ErrNotExist)) {
			return resources{}, fmt.Errorf("read cgroup CPU limit: %w", err)
		}
		if err == nil {
			fields := strings.Fields(string(cpu))
			if len(fields) != 2 {
				return resources{}, errors.New("invalid cpu.max")
			}
			period, err := strconv.Atoi(fields[1])
			if err != nil || period <= 0 {
				return resources{}, errors.New("invalid cpu.max period")
			}
			if fields[0] != "max" {
				quota, err := strconv.Atoi(fields[0])
				if err != nil || quota < 0 {
					return resources{}, errors.New("invalid cpu.max quota")
				}
				r.cpus = min(r.cpus, quota/period)
			}
		}
		for _, name := range []string{"memory.max", "memory.high"} {
			data, err := fs.ReadFile(fsys, path.Join(group, name))
			if group == "." && errors.Is(err, fs.ErrNotExist) {
				continue // The host's cgroup root has no resource limit files.
			}
			if err != nil {
				return resources{}, fmt.Errorf("read cgroup memory limit: %w", err)
			}
			if strings.TrimSpace(string(data)) == "max" {
				continue
			}
			limit, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
			if err != nil {
				return resources{}, fmt.Errorf("parse cgroup memory limit: %w", err)
			}
			data, err = fs.ReadFile(fsys, path.Join(group, "memory.current"))
			if err != nil {
				return resources{}, fmt.Errorf("read cgroup memory usage: %w", err)
			}
			used, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
			if err != nil {
				return resources{}, fmt.Errorf("parse cgroup memory usage: %w", err)
			}
			r.available = min(r.available, limit-min(limit, used))
		}
		if group == "." {
			return r, nil
		}
		group = path.Dir(group)
	}
}

func detect() (resources, error) {
	// Respect affinity, the caller's GOMAXPROCS, and the runtime's cgroup quota
	// even when an explicit GOMAXPROCS would otherwise override that quota.
	cpus := min(runtime.NumCPU(), runtime.GOMAXPROCS(0))
	runtime.SetDefaultGOMAXPROCS()
	cpus = min(cpus, runtime.GOMAXPROCS(0))
	memory, err := mem.VirtualMemory()
	if err != nil {
		return resources{}, fmt.Errorf("read available memory: %w", err)
	}
	r := resources{cpus, memory.Available}
	switch runtime.GOOS {
	case "darwin":
		return r, nil
	case "linux":
		data, err := os.ReadFile("/proc/self/cgroup")
		if err != nil {
			return resources{}, fmt.Errorf("read process cgroup: %w", err)
		}
		for line := range strings.SplitSeq(string(data), "\n") {
			if group, ok := strings.CutPrefix(line, "0::/"); ok {
				if group == "" {
					group = "."
				}
				return limitCgroup(os.DirFS("/sys/fs/cgroup"), group, r)
			}
		}
	}
	return resources{}, errors.New("resource limits could not be determined")
}

func main() {
	packages := flag.Int("packages", 4, "number of concurrent sharded packages")
	flag.Parse()
	r, err := detect()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Resource detection unavailable; using standard test scheduling")
		r = resources{}
	}
	// Emit the settings together so make uses the same per-process budget and
	// remainder concurrency that the shard calculation reserves.
	fmt.Println(shardBudget(r, *packages), processProcs, remainderParallel)
}
