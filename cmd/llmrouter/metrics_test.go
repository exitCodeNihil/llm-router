package main

import (
	"strings"
	"testing"

	"github.com/exitcodenihil/llm-router/internal/metrics"
	"github.com/exitcodenihil/llm-router/internal/telemetry"
)

// The alert rule in the chart fires on llmr_telemetry_destination_failing by
// destination, so that series must exist for every destination.
func TestDestinationMetrics(t *testing.T) {
	var w metrics.PromWriter
	destinationMetrics(&w, []telemetry.Health{
		{Name: "tempo", Kind: "otlp", Status: "healthy", Queued: 3, Delivered: 10},
		{Name: `lf "eu"`, Kind: "langfuse", Status: "failing", Dropped: telemetry.Dropped{Rejected: 2}},
	})
	out := string(w.Bytes())
	for _, want := range []string{
		`llmr_telemetry_destination_failing{destination="tempo",kind="otlp"} 0`,
		`llmr_telemetry_destination_failing{destination="lf \"eu\"",kind="langfuse"} 1`,
		`llmr_telemetry_destination_queued{destination="tempo"} 3`,
		`llmr_telemetry_destination_delivered_total{destination="tempo"} 10`,
		`llmr_telemetry_destination_dropped_total{destination="lf \"eu\"",reason="rejected"} 2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
	if n := strings.Count(out, "# TYPE llmr_telemetry_destination_failing "); n != 1 {
		t.Errorf("family announced %d times", n)
	}
}
