package index

import (
	"runtime/metrics"
	"sync/atomic"
	"time"
)

// heapSampler records the peak live heap observed while it runs, so a Build
// can report what its decode-cache budget actually cost in memory (see
// Stats.PeakHeapBytes); Options.DecodeCacheBudget is a blob-byte proxy whose
// real heap footprint is only knowable by measuring it.
type heapSampler struct {
	peak atomic.Uint64
	stop chan struct{}
	done chan struct{}
}

// startHeapSampler samples the live heap every interval until stop is called.
func startHeapSampler(interval time.Duration) *heapSampler {
	s := &heapSampler{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		samples := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			metrics.Read(samples)
			if samples[0].Value.Kind() == metrics.KindUint64 {
				if v := samples[0].Value.Uint64(); v > s.peak.Load() {
					s.peak.Store(v)
				}
			}
			select {
			case <-s.stop:
				return
			case <-ticker.C:
			}
		}
	}()
	return s
}

// stop ends sampling and returns the peak live heap in bytes seen so far.
func (s *heapSampler) stopAndPeak() uint64 {
	close(s.stop)
	<-s.done
	return s.peak.Load()
}
