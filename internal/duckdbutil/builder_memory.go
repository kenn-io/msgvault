package duckdbutil

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"

	"github.com/shirou/gopsutil/v4/mem"
)

// builderMemoryBudget leaves headroom for the daemon, other applications and
// allocations outside DuckDB's buffer manager. The floor is an attempt budget;
// it cannot guarantee that a build will succeed on an exhausted host.
func builderMemoryBudget(available uint64) uint64 {
	return min(uint64(16<<30), max(uint64(256<<20), available/4))
}

func defaultBuilderMemoryLimit() string {
	memory, err := mem.VirtualMemory()
	if err != nil || memory.Total == 0 {
		return "2GB"
	}
	available := memory.Available
	if runtime.GOOS == "linux" {
		available, err = cgroupAvailableMemory(os.DirFS("/"), available)
	}
	budget := builderMemoryBudget(available)
	if err != nil {
		// Unknown container topology must not enable a larger default. Retain
		// any lower bound successfully read before discovery failed.
		budget = min(budget, 2_000_000_000)
	}
	return strconv.FormatUint(budget, 10) + "B"
}

// cgroupAvailableMemory returns the lower of host availability and remaining
// memory under each visible cgroup v2 ancestor. On error, the returned value
// still includes every successfully read bound.
func cgroupAvailableMemory(fsys fs.FS, available uint64) (uint64, error) {
	root, group, err := cgroupMemoryMount(fsys)
	if err != nil {
		return available, err
	}
	var discoveryErr error
	for {
		for _, name := range []string{"memory.max", "memory.high"} {
			data, err := fs.ReadFile(fsys, path.Join(group, name))
			if group == root && errors.Is(err, fs.ErrNotExist) {
				continue // The host's root cgroup has no memory controller files.
			}
			if err != nil {
				discoveryErr = errors.Join(discoveryErr, err)
				continue
			}
			if strings.TrimSpace(string(data)) == "max" {
				continue
			}
			limit, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
			if err != nil {
				discoveryErr = errors.Join(discoveryErr, err)
				continue
			}
			data, err = fs.ReadFile(fsys, path.Join(group, "memory.current"))
			if err != nil {
				discoveryErr = errors.Join(discoveryErr, err)
				continue
			}
			used, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
			if err != nil {
				discoveryErr = errors.Join(discoveryErr, err)
				continue
			}
			available = min(available, limit-min(limit, used))
		}
		if group == root {
			return available, discoveryErr
		}
		group = path.Dir(group)
	}
}

// cgroupMemoryMount maps process membership onto its visible mount, including
// mounts whose root is an ancestor below the host's cgroup root.
func cgroupMemoryMount(fsys fs.FS) (root, group string, err error) {
	data, err := fs.ReadFile(fsys, "proc/self/cgroup")
	if err != nil {
		return "", "", fmt.Errorf("read cgroup membership: %w", err)
	}
	var membership string
	for line := range strings.SplitSeq(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, "0::"); ok {
			membership = value
			break
		}
	}
	if !path.IsAbs(membership) || path.Clean(membership) != membership {
		return "", "", errors.New("unknown cgroup v2 membership")
	}
	data, err = fs.ReadFile(fsys, "proc/self/mountinfo")
	if err != nil {
		return "", "", fmt.Errorf("read cgroup mounts: %w", err)
	}
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	var widestRoot string
	for line := range strings.SplitSeq(string(data), "\n") {
		before, after, ok := strings.Cut(line, " - ")
		if !ok || !strings.HasPrefix(after, "cgroup2 ") {
			continue
		}
		fields := strings.Fields(before)
		if len(fields) < 6 {
			continue
		}
		mountRoot, mountPoint := unescape.Replace(fields[3]), unescape.Replace(fields[4])
		if !path.IsAbs(mountRoot) || !path.IsAbs(mountPoint) {
			continue
		}
		var relative string
		switch {
		case membership == mountRoot:
			relative = "."
		case strings.HasPrefix(membership, strings.TrimSuffix(mountRoot, "/")+"/"):
			relative = strings.TrimPrefix(membership, strings.TrimSuffix(mountRoot, "/")+"/")
		default:
			continue
		}
		candidateRoot := strings.TrimPrefix(mountPoint, "/")
		candidateGroup := path.Join(candidateRoot, relative)
		if fs.ValidPath(candidateRoot) && fs.ValidPath(candidateGroup) &&
			(widestRoot == "" || len(mountRoot) < len(widestRoot)) {
			// A subtree bind mount must not hide ancestors exposed by another mount.
			widestRoot, root, group = mountRoot, candidateRoot, candidateGroup
		}
	}
	if widestRoot != "" {
		return root, group, nil
	}
	return "", "", fmt.Errorf("no visible cgroup v2 mount for %s", membership)
}
