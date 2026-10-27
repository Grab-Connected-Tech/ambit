package runtime

import (
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// setMemoryLimit gives the Go runtime the container's memory limit, so the collector
// works harder before the kernel kills the process (§11.2). An explicit GOMEMLIMIT wins.
// It keeps 10% headroom for memory the Go runtime does not account for.
func setMemoryLimit() int64 {
	if os.Getenv("GOMEMLIMIT") != "" {
		return 0
	}
	for _, path := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		limit := cgroupLimit(string(raw))
		if limit > 0 {
			debug.SetMemoryLimit(limit / 10 * 9)
			return limit / 10 * 9
		}
	}
	return 0
}

// cgroupLimit reads a cgroup memory limit; "max" and the v1 "unlimited" sentinel
// (a page-rounded maximum int64) mean no limit.
func cgroupLimit(s string) int64 {
	s = strings.TrimSpace(s)
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 || n >= 1<<62 {
		return 0
	}
	return n
}
