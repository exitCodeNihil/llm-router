package metrics

import (
	"os"
	"runtime"
	rtmetrics "runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Memory is what this process uses of its container, read as the Rust
// gateway reads it (gateway/src/resources.rs): the cgroup v2 working set
// (memory.current less inactive page cache) against memory.max, else the
// process's RSS. Limit is 0 when there is none.
func Memory() (used, limit int64) {
	if dir := cgroupDir(); dir != "" {
		if cur, ok := readNumber(dir + "/memory.current"); ok {
			inactive, _ := readField(dir+"/memory.stat", "inactive_file")
			limit, _ = readNumber(dir + "/memory.max")
			return max(cur-inactive, 0), limit
		}
	}
	if b, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if f := strings.Fields(l); len(f) >= 2 && f[0] == "VmRSS:" {
				kb, _ := strconv.ParseInt(f[1], 10, 64)
				return kb * 1024, 0
			}
		}
	}
	// Not Linux: what the Go runtime has mapped and not given back.
	s := []rtmetrics.Sample{{Name: "/memory/classes/total:bytes"}, {Name: "/memory/classes/heap/released:bytes"}}
	rtmetrics.Read(s)
	return int64(value(s[0]) - value(s[1])), 0
}

var cgroupDir = sync.OnceValue(func() string {
	var dirs []string
	if b, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if p, ok := strings.CutPrefix(l, "0::"); ok {
				dirs = append(dirs, "/sys/fs/cgroup"+strings.TrimSuffix(p, "/"))
			}
		}
	}
	for _, d := range append(dirs, "/sys/fs/cgroup") {
		if _, err := os.Stat(d + "/memory.current"); err == nil {
			return d
		}
	}
	return ""
})

// readNumber reads a cgroup value file; "max" (no limit) is not ok.
func readNumber(path string) (int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n, err == nil
}

// readField reads one field of a "key value" file such as memory.stat.
func readField(path, key string) (int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, key+" "); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

func value(s rtmetrics.Sample) uint64 {
	switch s.Value.Kind() {
	case rtmetrics.KindUint64:
		return s.Value.Uint64()
	case rtmetrics.KindFloat64:
		return uint64(s.Value.Float64())
	}
	return 0
}

var cpu struct {
	sync.Mutex
	at        time.Time
	used, pct float64
}

// CPUPct is this process's CPU use as a percentage of the CPUs it may use
// (GOMAXPROCS follows the container's CPU limit), averaged over at least the
// last five seconds.
func CPUPct() float64 {
	used := cpuSeconds()
	now := time.Now()
	cpu.Lock()
	defer cpu.Unlock()
	switch {
	case cpu.at.IsZero():
		cpu.at, cpu.used = now, used
	case now.Sub(cpu.at) >= 5*time.Second:
		wall := now.Sub(cpu.at).Seconds()
		cpu.pct = min(max((used-cpu.used)/wall/float64(runtime.GOMAXPROCS(0))*100, 0), 100)
		cpu.at, cpu.used = now, used
	}
	return cpu.pct
}

// ProcessCollector writes the control plane's memory and Go runtime families.
func ProcessCollector(version string) Collector {
	return CollectorFunc(func(w *PromWriter) {
		used, limit := Memory()
		w.Gauge("llmr_memory_bytes", "Memory in use: the cgroup's working set, or the process's RSS", float64(used))
		if limit > 0 {
			w.Gauge("llmr_memory_limit_bytes", "The cgroup's memory limit (absent: no limit)", float64(limit))
		}
		s := []rtmetrics.Sample{
			{Name: "/memory/classes/heap/objects:bytes"},
			{Name: "/memory/classes/total:bytes"},
			{Name: "/sched/goroutines:goroutines"},
			{Name: "/gc/cycles/total:gc-cycles"},
		}
		rtmetrics.Read(s)
		w.Gauge("llmr_go_heap_bytes", "Bytes of live and not yet swept heap objects", float64(value(s[0])))
		w.Gauge("llmr_go_mapped_bytes", "Memory mapped by the Go runtime", float64(value(s[1])))
		w.Gauge("llmr_go_goroutines", "Goroutines", float64(value(s[2])))
		w.Counter("llmr_go_gc_cycles_total", "Completed GC cycles", float64(value(s[3])))
		w.Gauge("llmr_build_info", "The control plane's version", 1, "runtime", "control_plane", "version", version)
	})
}
