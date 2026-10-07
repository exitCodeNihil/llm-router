package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

var gateways = []string{"router", "lrust", "litellm", "otari"}

var gatewayNames = map[string]string{"router": "llm-router", "lrust": "LiteLLM Rust", "litellm": "LiteLLM", "otari": "Otari", "direct": "direct"}

// run is one loadgen summary plus what run.sh recorded beside it.
type run struct {
	summary
	Unsupported bool `json:"unsupported"`
	db          *struct{ Rows, Xacts, WALBytes int }
	after       map[string]any
}

type results struct {
	dir  string
	runs map[string]*run
}

func load(dir string) (*results, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	r := &results{dir: dir, runs: map[string]*run{}}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		if strings.HasSuffix(name, ".db") || strings.HasSuffix(name, ".after") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var x run
		if json.Unmarshal(b, &x) != nil {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(dir, name+".db.json")); err == nil {
			x.db = &struct{ Rows, Xacts, WALBytes int }{}
			var d struct {
				Rows     int `json:"rows"`
				Xacts    int `json:"xacts"`
				WALBytes int `json:"wal_bytes"`
			}
			json.Unmarshal(b, &d)
			*x.db = struct{ Rows, Xacts, WALBytes int }{d.Rows, d.Xacts, d.WALBytes}
		}
		if b, err := os.ReadFile(filepath.Join(dir, name+".after.json")); err == nil {
			json.Unmarshal(b, &x.after)
		}
		r.runs[name] = &x
	}
	return r, nil
}

// res is a run's CPU and memory for one container over its measured window.
func (r *results) res(x *run, svc string) (resources, bool) {
	if x == nil || x.Tags["samples"] == "" {
		return resources{}, false
	}
	u, err := usageIn(filepath.Join(r.dir, x.Tags["samples"]), svc, float64(x.WindowStartMS)/1000, float64(x.WindowEndMS)/1000)
	return u, err == nil
}

// repeats returns the runs named prefix-r1, prefix-r2, ... (or just prefix).
func (r *results) repeats(prefix string) []*run {
	var out []*run
	for i := 1; ; i++ {
		x, ok := r.runs[fmt.Sprintf("%s-r%d", prefix, i)]
		if !ok {
			break
		}
		out = append(out, x)
	}
	if x, ok := r.runs[prefix]; ok && len(out) == 0 {
		out = append(out, x)
	}
	return out
}

// med is the median of f over repeats, skipping runs that didn't produce the number.
func med(runs []*run, f func(*run) (float64, bool)) (float64, bool) {
	var v []float64
	for _, x := range runs {
		if x.Unsupported {
			continue
		}
		if n, ok := f(x); ok {
			v = append(v, n)
		}
	}
	if len(v) == 0 {
		return 0, false
	}
	slices.Sort(v)
	return v[len(v)/2], true
}

func errs(x *run) int {
	var n int
	for _, e := range x.Errors {
		n += e
	}
	return n
}

func healthy(x *run) bool {
	var errs int
	for _, n := range x.Errors {
		errs += n
	}
	return x.Sent > 0 && errs <= x.Sent/100 && x.Achieved >= 0.95*x.Rate
}

// cell formats a number, or n/a, or "overloaded" when the run failed.
func cell(runs []*run, f func(*run) (float64, bool), format string) string {
	if len(runs) == 0 {
		return "–"
	}
	if runs[0].Unsupported {
		return "n/a"
	}
	if !slices.ContainsFunc(runs, healthy) {
		return "overloaded"
	}
	v, ok := med(slices.DeleteFunc(slices.Clone(runs), func(x *run) bool { return !healthy(x) }), f)
	if !ok {
		return "–"
	}
	return fmt.Sprintf(format, v)
}

func (r *results) file(name string) map[string]any {
	b, err := os.ReadFile(filepath.Join(r.dir, name))
	if err != nil {
		return nil
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func (r *results) maxRate(gw, cpus string) string {
	b, err := os.ReadFile(filepath.Join(r.dir, "max-"+cpus+"-"+gw))
	if err != nil {
		return "–"
	}
	return strings.TrimSpace(string(b))
}

// summarize prints the headline and detail tables for docs/benchmarks.md.
func summarizeDir(dir string, w io.Writer) error {
	r, err := load(dir)
	if err != nil {
		return err
	}
	row := func(label string, f func(gw string) string) {
		fmt.Fprintf(w, "| %s |", label)
		for _, gw := range gateways {
			fmt.Fprintf(w, " %s |", f(gw))
		}
		fmt.Fprintln(w)
	}
	ovh := func(q string) func(*run) (float64, bool) {
		return func(x *run) (float64, bool) { v, ok := x.Overhead[q]; return v, ok }
	}
	ttft := func(q string) func(*run) (float64, bool) {
		return func(x *run) (float64, bool) { v, ok := x.OverheadTTFT[q]; return v, ok }
	}
	cpuPerReq := func(svc string) func(*run) (float64, bool) {
		return func(x *run) (float64, bool) {
			u, ok := r.res(x, svc)
			return u.CPUSec * 1000 / float64(x.Sent), ok && x.Sent > 0
		}
	}

	fmt.Fprintln(w, "| | llm-router | LiteLLM Rust | LiteLLM | Otari |")
	fmt.Fprintln(w, "|---|---|---|---|---|")
	row("Image, download size", func(gw string) string {
		m := r.file("s1-" + gw + ".json")
		if m == nil {
			return "–"
		}
		if c, ok := m["compressed_bytes"].(float64); ok {
			return fmt.Sprintf("%.0f MB", c/1e6)
		}
		return fmt.Sprintf("%.0f MB unpacked", m["image_bytes"].(float64)/1e6)
	})
	row("Restart to first proxied request", func(gw string) string {
		m := r.file("s1-" + gw + ".json")
		if m == nil {
			return "–"
		}
		var v []float64
		for _, x := range m["restart_ms"].([]any) {
			v = append(v, x.(float64))
		}
		slices.Sort(v)
		return fmt.Sprintf("%.1f s", v[len(v)/2]/1000)
	})
	row("Idle memory", func(gw string) string {
		m := r.file("s1-" + gw + ".json")
		if m == nil {
			return "–"
		}
		u, err := usageIn(filepath.Join(dir, m["samples"].(string)), gw, m["idle_from_ms"].(float64)/1000, m["idle_to_ms"].(float64)/1000)
		if err != nil {
			return "–"
		}
		return fmt.Sprintf("%.0f MiB", u.MemAvgMiB)
	})
	row("Max sustainable req/s, 1 vCPU", func(gw string) string { return r.maxRate(gw, "1") })
	row("Max sustainable req/s, 2 vCPU", func(gw string) string { return r.maxRate(gw, "2") })
	row("Max sustainable req/s, 4 vCPU", func(gw string) string { return r.maxRate(gw, "4") })
	row("Added latency p50 / p99, 100 req/s", func(gw string) string {
		x := r.repeats("s2-fixed-100-" + gw)
		return cell(x, ovh("p50"), "%.2f") + " / " + cell(x, ovh("p99"), "%.2f ms")
	})
	row("Added latency, one request at a time", func(gw string) string {
		return cell(r.repeats("s2c-fast-1-"+gw), ovh("p50"), "%.2f ms")
	})
	row("CPU per request, 100 req/s", func(gw string) string {
		return cell(r.repeats("s2-fixed-100-"+gw), cpuPerReq(gw), "%.2f ms")
	})
	row("Postgres CPU per request, 100 req/s", func(gw string) string {
		if gw == "lrust" {
			return "n/a"
		}
		return cell(r.repeats("s2-fixed-100-"+gw), cpuPerReq("pg"), "%.2f ms")
	})
	row("Streaming: added time to first token, 10 streams/s", func(gw string) string {
		return cell(r.repeats("s3-chat-10-"+gw), ttft("p50"), "%.2f ms")
	})
	row("Streaming: chunk delay p99, 50 streams/s", func(gw string) string {
		return cell(r.repeats("s3-chat-50-"+gw), func(x *run) (float64, bool) { v, ok := x.ChunkDelay["p99"]; return v, ok }, "%.2f ms")
	})
	row("Claude Code request (60 KB, streamed): added time to first token, 10/s", func(gw string) string {
		return cell(r.repeats("s5-cc-10-"+gw), ttft("p50"), "%.2f ms")
	})
	row("Memory with 1,000 streams open (peak)", func(gw string) string {
		x := r.runs["s3-open-1000-"+gw]
		if x == nil || x.Unsupported {
			return "–"
		}
		if !healthy(x) {
			return "overloaded"
		}
		u, ok := r.res(x, gw)
		if !ok {
			return "–"
		}
		return fmt.Sprintf("%.0f MiB", u.MemPeakMiB)
	})
	row("Usage rows recorded / requests served", func(gw string) string {
		if gw == "lrust" {
			return "n/a (no usage logging)"
		}
		var rows, ok int
		for _, x := range r.runs {
			if x.db != nil && x.Tags["gw"] == gw {
				rows += x.db.Rows
				ok += x.OKAll
			}
		}
		if ok == 0 {
			return "–"
		}
		return fmt.Sprintf("%.2f%%", 100*float64(rows)/float64(ok))
	})
	stopped := func(test string) func(string) string {
		return func(gw string) string {
			x := r.runs[test+"-"+gw]
			if x == nil || x.after == nil {
				return "–"
			}
			if x.Unsupported {
				return "n/a"
			}
			sent, _ := x.Mock["requests"].(float64)
			if n, _ := x.after["in_flight"].(float64); n == 0 {
				return "yes"
			}
			return fmt.Sprintf("**no**, %v of %.0f still open 2 s later", x.after["in_flight"], sent)
		}
	}
	row("Client hangs up mid-stream: upstream call stopped", stopped("s7-cancel-stream"))
	row("Client hangs up before the first byte: upstream call stopped", stopped("s7-cancel-slow"))
	row("Fails over from a 500, a 429 and a flaky upstream", func(gw string) string {
		var bad []string
		for _, f := range []string{"fo500", "fo429", "foflaky"} {
			x := r.runs["s7-"+f+"-"+gw]
			if x == nil {
				return "n/a (no fallbacks)"
			}
			if x.OK < x.Sent {
				bad = append(bad, f)
			}
		}
		if bad != nil {
			return "**no**: " + strings.Join(bad, ", ")
		}
		return "yes"
	})
	row("Fails over from a hung upstream", func(gw string) string {
		x := r.runs["s7-fohang-"+gw]
		switch {
		case x == nil:
			return "n/a (no fallbacks)"
		case x.OK == x.Sent:
			return fmt.Sprintf("yes, after %.1f s", x.Latency["p50"]/1000)
		default:
			return "**no** (no upstream timeout)"
		}
	})
	row("Passes the upstream's Retry-After on a 429", func(gw string) string {
		b, err := os.ReadFile(filepath.Join(dir, "s7-429-"+gw+".txt"))
		switch {
		case err != nil:
			return "–"
		case strings.Contains("\n"+strings.ToLower(string(b)), "\nretry-after:"):
			return "yes"
		case strings.Contains(strings.ToLower(string(b)), "retry-after:"):
			// LiteLLM renames it (llm_provider-retry-after), which clients don't read.
			return "renamed, so clients miss it"
		default:
			return "no"
		}
	})
	row("6,000 requests waiting 30 s on a slow upstream", func(gw string) string {
		x := r.runs["s7-slow-"+gw]
		if x == nil {
			return "–"
		}
		return fmt.Sprintf("%d of %d ok", x.OKAll, x.OKAll+errs(x))
	})

	// Detail: the 1-vCPU ladder.
	fmt.Fprint(w, "\n### Non-streaming ladder, 1 vCPU (added latency p50 / p99 ms)\n\n")
	fmt.Fprintln(w, "| req/s | "+strings.Join(names(), " | ")+" |")
	fmt.Fprintln(w, "|---|"+strings.Repeat("---|", len(gateways)+1))
	var rates []int
	for name := range r.runs {
		if rest, ok := strings.CutPrefix(name, "s2-ladder-1-"); ok {
			if i := strings.LastIndexByte(rest, '-'); i > 0 {
				if n, err := strconv.Atoi(rest[i+1:]); err == nil && !slices.Contains(rates, n) {
					rates = append(rates, n)
				}
			}
		}
	}
	slices.Sort(rates)
	for _, rate := range rates {
		fmt.Fprintf(w, "| %d |", rate)
		for _, gw := range append([]string{"direct"}, gateways...) {
			x := r.runs[fmt.Sprintf("s2-ladder-1-%s-%d", gw, rate)]
			switch {
			case x == nil:
				fmt.Fprint(w, " |")
			case !healthy(x):
				fmt.Fprint(w, " overloaded |")
			default:
				fmt.Fprintf(w, " %.2f / %.2f |", x.Overhead["p50"], x.Overhead["p99"])
			}
		}
		fmt.Fprintln(w)
	}

	// Detail: fixed concurrency, the shape of an earlier independent study.
	fmt.Fprint(w, "\n### Fixed concurrency (added latency p50 / p99 ms, req/s)\n\n")
	fmt.Fprintln(w, "| test | "+strings.Join(names(), " | ")+" |")
	fmt.Fprintln(w, "|---|"+strings.Repeat("---|", len(gateways)+1))
	for _, t := range []string{"fast-1", "fast-10", "fast-50", "fast-100", "tok10-1", "tok10-10", "tok10-50", "tok0-1", "tok0-10", "tok0-50"} {
		fmt.Fprintf(w, "| %s |", t)
		for _, gw := range append([]string{"direct"}, gateways...) {
			x := r.runs["s2c-"+t+"-"+gw]
			switch {
			case x == nil:
				fmt.Fprint(w, " |")
			case x.Unsupported:
				fmt.Fprint(w, " n/a |")
			case strings.HasPrefix(t, "tok"):
				fmt.Fprintf(w, " ttft +%.2f, delay p99 %.2f, batched %.0f%%, %.0f/s |", x.OverheadTTFT["p50"], x.ChunkDelay["p99"], 100*x.Batched, x.Achieved)
			default:
				fmt.Fprintf(w, " %.2f / %.2f, %.0f/s |", x.Overhead["p50"], x.Overhead["p99"], x.Achieved)
			}
		}
		fmt.Fprintln(w)
	}
	return nil
}

func names() []string {
	out := []string{"direct"}
	for _, gw := range gateways {
		out = append(out, gatewayNames[gw])
	}
	return out
}
