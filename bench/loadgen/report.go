package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// report prints every run summary in dir as one markdown table, with the gateway's and
// Postgres's CPU and memory over each run's window. The curated tables in docs/benchmarks.md
// are built from these numbers.
func report(dir string, w io.Writer) error {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	fmt.Fprintln(w, "| run | gw | cpus | rate/conc | achieved | ok | errors | overhead p50 | overhead p99 | latency p99 | ttft ovh p50 | ttft ovh p99 | gap p99 | chunk delay p99 | batched | gw cpu ms/req | pg cpu ms/req | gw mem MiB | usage rows |")
	fmt.Fprintln(w, "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		var s summary
		if json.Unmarshal(b, &s) != nil || s.Sent == 0 {
			continue
		}
		var errs []string
		for k, v := range s.Errors {
			errs = append(errs, fmt.Sprintf("%s:%d", k, v))
		}
		sort.Strings(errs)
		load := fmt.Sprintf("%.0f/s", s.Rate)
		if s.Concurrency > 0 {
			load = fmt.Sprintf("c%d", s.Concurrency)
		}
		gwCPU, pgCPU, mem := "", "", ""
		if s.Tags["samples"] != "" && s.Tags["gw"] != "direct" {
			path := filepath.Join(dir, s.Tags["samples"])
			from, to := float64(s.WindowStartMS)/1000, float64(s.WindowEndMS)/1000
			if u, err := usageIn(path, s.Tags["gw"], from, to); err == nil {
				gwCPU, mem = fmt.Sprintf("%.3f", u.CPUSec*1000/float64(s.Sent)), fmt.Sprintf("%.0f", u.MemPeakMiB)
			}
			if u, err := usageIn(path, "pg", from, to); err == nil {
				pgCPU = fmt.Sprintf("%.3f", u.CPUSec*1000/float64(s.Sent))
			}
		}
		rows := ""
		if db, err := os.ReadFile(strings.TrimSuffix(f, ".json") + ".db.json"); err == nil {
			var d struct{ Rows int }
			json.Unmarshal(db, &d)
			rows = fmt.Sprintf("%d/%d", d.Rows, s.OKAll)
		}
		fmt.Fprintf(w, "| %s | %s | %s | %s | %.1f | %d/%d | %s | %.3f | %.3f | %.1f | %.3f | %.3f | %.1f | %.2f | %.1f%% | %s | %s | %s | %s |\n",
			strings.TrimSuffix(filepath.Base(f), ".json"), s.Tags["gw"], s.Tags["cpus"], load, s.Achieved, s.OK, s.Sent,
			strings.Join(errs, " "), s.Overhead["p50"], s.Overhead["p99"], s.Latency["p99"],
			s.OverheadTTFT["p50"], s.OverheadTTFT["p99"], s.Gap["p99"], s.ChunkDelay["p99"], s.Batched*100,
			gwCPU, pgCPU, mem, rows)
	}
	return nil
}

// resources is one container's resource use over a window, from the sampler's 1 Hz cgroup readings.
type resources struct {
	CPUSec      float64 `json:"cpu_sec"`
	Cores       float64 `json:"cores"`
	MemPeakMiB  float64 `json:"mem_peak_mib"` // working set: memory.current minus inactive file cache
	MemAvgMiB   float64 `json:"mem_avg_mib"`
	AnonPeakMiB float64 `json:"anon_peak_mib"`
	ThrottledMS float64 `json:"throttled_ms_per_s"` // time the CPU quota held it back, per second
	VMBusyMax   float64 `json:"vm_busy_max"`        // the busiest second of the whole VM, 0..1
	Seconds     float64 `json:"seconds"`
}

// usageIn reads a samples file for the container whose compose service is svc (llmr-bench-<svc>-1)
// between from and to (unix seconds; 0, 0 for the whole file).
func usageIn(path, svc string, from, to float64) (resources, error) {
	f, err := os.Open(path)
	if err != nil {
		return resources{}, err
	}
	defer f.Close()
	if to == 0 {
		to = math.MaxFloat64
	}
	var u resources
	var first, last []float64 // ts, cpu_usec, throttled_usec
	var memSum float64
	var n int
	var vmPrev []float64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		ts, err := strconv.ParseFloat(fields[0], 64)
		if err != nil || ts < from || ts > to {
			continue
		}
		v := make([]float64, len(fields)-2)
		for i, x := range fields[2:] {
			v[i], _ = strconv.ParseFloat(x, 64)
		}
		if fields[1] == "vm" {
			// user nice system idle iowait irq softirq steal ...
			if vmPrev != nil && len(v) >= 5 {
				var total, idle float64
				for i := range v {
					total += v[i] - vmPrev[i]
				}
				idle = v[3] - vmPrev[3] + v[4] - vmPrev[4]
				if total > 0 {
					u.VMBusyMax = max(u.VMBusyMax, 1-idle/total)
				}
			}
			vmPrev = v
			continue
		}
		if !strings.Contains(fields[1], "-"+svc+"-") || len(v) < 7 {
			continue
		}
		// cpu_usec throttled_usec nr_throttled mem anon inactive_file sock
		if first == nil {
			first = []float64{ts, v[0], v[1]}
		}
		last = []float64{ts, v[0], v[1]}
		ws := (v[3] - v[5]) / (1 << 20)
		u.MemPeakMiB = max(u.MemPeakMiB, ws)
		u.AnonPeakMiB = max(u.AnonPeakMiB, v[4]/(1<<20))
		memSum += ws
		n++
	}
	if n < 2 {
		return u, fmt.Errorf("no samples for %s in %s", svc, path)
	}
	u.Seconds = last[0] - first[0]
	u.CPUSec = (last[1] - first[1]) / 1e6
	u.Cores = u.CPUSec / u.Seconds
	u.ThrottledMS = (last[2] - first[2]) / 1e3 / u.Seconds
	u.MemAvgMiB = memSum / float64(n)
	return u, sc.Err()
}

// usageCmd: loadgen usage FILE SERVICE [FROM_MS TO_MS]
func usageCmd(args []string) error {
	var from, to float64
	if len(args) == 4 {
		a, _ := strconv.ParseFloat(args[2], 64)
		b, _ := strconv.ParseFloat(args[3], 64)
		from, to = a/1000, b/1000
	}
	u, err := usageIn(args[0], args[1], from, to)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(u)
	fmt.Println(string(b))
	return nil
}
