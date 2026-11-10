package metrics

import "testing"

func TestReplaceKeepsOneLineForARunOfTheSameEvent(t *testing.T) {
	var e Events
	e.Add("warn", "version skew")
	for _, v := range []string{"7", "8", "9"} {
		e.Replace("config epoch ", "info", "config epoch "+v+" applied")
	}
	e.Add("warn", "upstream p50 above SLO")
	e.Replace("config epoch ", "info", "config epoch 10 applied")

	var got []string
	for _, ev := range e.Snapshot() {
		got = append(got, ev.Text)
	}
	want := []string{"config epoch 10 applied", "upstream p50 above SLO", "config epoch 9 applied", "version skew"}
	if len(got) != len(want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %q, want %q", got, want)
		}
	}
}
