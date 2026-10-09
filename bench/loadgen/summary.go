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

// gatewayOrder is the column order; a gateway gets a column only when the results have runs for it.
var gatewayOrder = []string{"router", "rgw", "lrust", "litellm", "otari"}

var gatewayNames = map[string]string{"router": "llm-router", "rgw": "llm-router Rust gateway", "lrust": "LiteLLM Rust", "litellm": "LiteLLM", "otari": "Otari", "direct": "direct"}

// run is one loadgen summary plus what run.sh recorded beside it.
type run struct {
	summary
	Unsupported bool `json:"unsupported"`
	db          *struct {
		Rows             int `json:"rows"`
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	}
	after map[string]any
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
			json.Unmarshal(b, &x.db)
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

// gateways lists the gateways the results have runs for, in column order.
func (r *results) gateways() []string {
	var out []string
	for _, x := range r.runs {
		if gw := x.Tags["gw"]; gw != "" && gw != "direct" && !slices.Contains(out, gw) {
			out = append(out, gw)
		}
	}
	rank := func(gw string) int {
		if i := slices.Index(gatewayOrder, gw); i >= 0 {
			return i
		}
		return len(gatewayOrder)
	}
	slices.SortFunc(out, func(a, b string) int {
		if c := rank(a) - rank(b); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	return out
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
	gateways := r.gateways()
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

	fmt.Fprintln(w, "| | "+strings.Join(names(gateways)[1:], " | ")+" |")
	fmt.Fprintln(w, "|---|"+strings.Repeat("---|", len(gateways)))
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
	// Rows only the Rust gateway has: its control plane takes every request's
	// usage, and its limits must hold across instances.
	rgwRow := func(label string, f func() string) {
		if slices.Contains(gateways, "rgw") {
			row(label, func(gw string) string {
				if gw != "rgw" {
					return "n/a"
				}
				return f()
			})
		}
	}
	rgwRow("Control plane CPU per request, 100 req/s", func() string {
		return cell(r.repeats("s2-fixed-100-rgw"), cpuPerReq("cp"), "%.2f ms")
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
	// Only runs where the client got every response: an overloaded gateway still finishes and logs
	// requests the client gave up on, and the cancel tests log the cancelled streams.
	row("Usage rows and tokens match the upstream's own counts", func(gw string) string {
		if gw == "lrust" {
			return "n/a (no usage logging)"
		}
		n, exact := 0, 0
		for _, x := range r.runs {
			if x.db == nil || x.Tags["gw"] != gw || x.OKAll == 0 || len(x.Errors) > 0 || x.Mock["requests"] == nil {
				continue
			}
			n++
			m := func(k string) int { v, _ := x.Mock[k].(float64); return int(v) }
			if x.db.Rows == m("requests") && x.db.PromptTokens == m("prompt_tokens") && x.db.CompletionTokens == m("completion_tokens") {
				exact++
			}
		}
		if n == 0 {
			return "–"
		}
		if exact == n {
			return fmt.Sprintf("yes, exactly, in all %d runs", n)
		}
		return fmt.Sprintf("**%d of %d runs** exact", exact, n)
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
	letThrough := func(names ...string) string {
		ok, all := 0, 0
		for _, n := range names {
			x := r.runs[n]
			if x == nil {
				return "–"
			}
			ok, all = ok+x.OKAll, all+x.OKAll+errs(x)
		}
		return fmt.Sprintf("%d of %d", ok, all)
	}
	row("600/min limit, 1,200 sent in a minute: let through", func(gw string) string {
		if gw == "lrust" {
			return "n/a (no limits)"
		}
		return letThrough("s9-rpm-" + gw)
	})
	row("$0.01 budget (about 14 requests' worth): let through", func(gw string) string {
		if gw == "lrust" {
			return "n/a (no budgets)"
		}
		return letThrough("s9-budget-" + gw)
	})
	rgwRow("Same limit, 1,200 sent through two instances: let through", func() string {
		return letThrough("s10-rpm-a", "s10-rpm-b")
	})
	rgwRow("Same budget, through two instances: let through", func() string {
		return letThrough("s10-budget-a", "s10-budget-b")
	})
	row("6,000 requests waiting 30 s on a slow upstream", func(gw string) string {
		x := r.runs["s7-slow-"+gw]
		if x == nil {
			return "–"
		}
		return fmt.Sprintf("%d of %d ok", x.OKAll, x.OKAll+errs(x))
	})
	// The soak: s8-fast-GW-N and s8-chat-GW-N run side by side, then a spike and 10 s recovery slices.
	soak := func(gw string) (segs []*run) {
		for i := 1; ; i++ {
			f, c := r.runs[fmt.Sprintf("s8-fast-%s-%d", gw, i)], r.runs[fmt.Sprintf("s8-chat-%s-%d", gw, i)]
			if f == nil || c == nil {
				return segs
			}
			segs = append(segs, f, c)
		}
	}
	row("Soak, 2 vCPU, 70% plain + 30% streamed: requests ok", func(gw string) string {
		var ok, all int
		for _, x := range soak(gw) {
			ok, all = ok+x.OKAll, all+x.OKAll+errs(x)
		}
		if all == 0 {
			return "–"
		}
		return fmt.Sprintf("%d of %d", ok, all)
	})
	row("Soak: memory, first minute → last minute", func(gw string) string {
		segs := soak(gw)
		if len(segs) == 0 || segs[0].Tags["samples"] == "" {
			return "–"
		}
		path := filepath.Join(dir, segs[0].Tags["samples"])
		from, to := float64(segs[0].WindowStartMS)/1000, float64(segs[len(segs)-1].WindowEndMS)/1000
		a, err1 := usageIn(path, gw, from, from+60)
		b, err2 := usageIn(path, gw, to-60, to)
		if err1 != nil || err2 != nil {
			return "–"
		}
		return fmt.Sprintf("%.0f → %.0f MiB", a.MemAvgMiB, b.MemAvgMiB)
	})
	row("Spike to twice its own 2 vCPU maximum, 30 s: requests ok", func(gw string) string {
		x := r.runs["s8-spike-"+gw]
		if x == nil {
			return "–"
		}
		return fmt.Sprintf("%.0f%%", 100*float64(x.OKAll)/float64(max(1, x.OKAll+errs(x))))
	})
	row("Back to normal after the spike", func(gw string) string {
		if r.runs["s8-spike-"+gw] == nil {
			return "–"
		}
		for i := 1; i <= 18; i++ {
			x := r.runs[fmt.Sprintf("s8-post-%s-%02d", gw, i)]
			if x != nil && errs(x) == 0 && x.Latency["p99"] <= 50 {
				return fmt.Sprintf("within %d s", 10*i)
			}
		}
		return "**not within 3 min**"
	})

	// Detail: the 1-vCPU ladder.
	fmt.Fprint(w, "\n### Non-streaming ladder, 1 vCPU (added latency p50 / p99 ms)\n\n")
	fmt.Fprintln(w, "| req/s | "+strings.Join(names(gateways), " | ")+" |")
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
	fmt.Fprintln(w, "| test | "+strings.Join(names(gateways), " | ")+" |")
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

func names(gateways []string) []string {
	out := []string{"direct"}
	for _, gw := range gateways {
		out = append(out, gatewayNames[gw])
	}
	return out
}

// spliceDoc writes the summary of dir into doc, between its "<!-- generated: ... -->" and
// "<!-- end generated -->" lines, leaving the rest of the document alone.
func spliceDoc(dir, doc string) error {
	b, err := os.ReadFile(doc)
	if err != nil {
		return err
	}
	text := string(b)
	start := strings.Index(text, "<!-- generated:")
	end := strings.Index(text, "<!-- end generated -->")
	if start < 0 || end < start {
		return fmt.Errorf("%s has no <!-- generated: ... --> ... <!-- end generated --> block", doc)
	}
	var out strings.Builder
	out.WriteString(text[:start])
	fmt.Fprintf(&out, "<!-- generated: go run ./loadgen summary results/%s (in bench/) -->\n", filepath.Base(dir))
	if err := summarizeDir(dir, &out); err != nil {
		return err
	}
	out.WriteString(text[end:])
	return os.WriteFile(doc, []byte(out.String()), 0o644)
}
