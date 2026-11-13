package usage

import "testing"

func TestHistBucket(t *testing.T) {
	for ms, want := range map[int]int{0: 0, 9: 0, 10: 1, 14: 1, 15: 2, 999: 12, 1000: 13, 499999: 28, 500000: 29, 9000000: 29} {
		if got := histBucket(ms); got != want {
			t.Errorf("histBucket(%d) = %d, want %d", ms, got, want)
		}
	}
}

func TestPercentile(t *testing.T) {
	counts := make([]int64, len(histBounds)+1)
	if got := Percentile(counts, 0.5); got != 0 {
		t.Errorf("no counts: %d, want 0", got)
	}
	counts[histBucket(120)] = 10 // 100-150 ms
	if got := Percentile(counts, 0.5); got != 125 {
		t.Errorf("median of one range = %d, want 125", got)
	}
	counts[histBucket(4000)] = 10 // 3500-5000 ms
	if got := Percentile(counts, 0.95); got < 3500 || got > 5000 {
		t.Errorf("p95 = %d, want within 3500-5000", got)
	}
	counts[len(counts)-1] = 1000 // 500 s and up
	if got := Percentile(counts, 0.99); got != 500000 {
		t.Errorf("p99 in the open range = %d, want 500000", got)
	}
}

func TestMoneyDropsFloatNoise(t *testing.T) {
	var sum float64
	for range 800 {
		sum += 0.0557525
	}
	if sum == 44.602 {
		t.Skip("this sum has no noise on this platform")
	}
	if got := money(sum); got != 44.602 {
		t.Fatalf("money(%v) = %v, want 44.602", sum, got)
	}
}
