//go:build !unix

package metrics

func cpuSeconds() float64 { return 0 }
