package index

import (
	"testing"
	"time"
)

func TestHeapSampler_ReportsPeakAndStops(t *testing.T) {
	s := startHeapSampler(time.Millisecond)
	// Keep something allocated so the live heap is observably non-zero.
	keep := make([]byte, 8<<20)
	time.Sleep(5 * time.Millisecond)
	peak := s.stopAndPeak()
	if peak == 0 {
		t.Fatal("peak live heap = 0, want > 0")
	}
	if uint64(len(keep)) > peak {
		t.Fatalf("peak %d smaller than a live %d-byte allocation", peak, len(keep))
	}
	select {
	case <-s.done:
	default:
		t.Fatal("sampler goroutine still running after stopAndPeak")
	}
}
