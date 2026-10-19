package fanout

import (
	"sort"
	"sync"
	"time"
)

// Tracker guarda las ultimas N latencias exitosas de un servicio y expone un
// percentil cacheado. El hedge delay se calcula con esto, asi cada servicio
// tiene su propio umbral sin configurarlo a mano.
type Tracker struct {
	mu        sync.Mutex
	window    []time.Duration
	next      int
	filled    bool
	sinceCalc int
	cached    time.Duration
	pct       float64
}

const (
	trackerSize    = 1000
	recalcEvery    = 50
	minSamplesWarm = 100
)

func NewTracker(pct float64) *Tracker {
	return &Tracker{window: make([]time.Duration, trackerSize), pct: pct}
}

func (t *Tracker) Add(d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.window[t.next] = d
	t.next = (t.next + 1) % trackerSize
	if t.next == 0 {
		t.filled = true
	}
	t.sinceCalc++
	if t.sinceCalc >= recalcEvery {
		t.sinceCalc = 0
		t.cached = t.compute()
	}
}

// Percentile devuelve el percentil cacheado y false si todavia no hay
// suficientes muestras (warm-up).
func (t *Tracker) Percentile() (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.next
	if t.filled {
		n = trackerSize
	}
	if n < minSamplesWarm {
		return 0, false
	}
	return t.cached, true
}

func (t *Tracker) compute() time.Duration {
	n := t.next
	if t.filled {
		n = trackerSize
	}
	if n == 0 {
		return 0
	}
	s := make([]time.Duration, n)
	copy(s, t.window[:n])
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[nearestRank(n, t.pct)]
}

func nearestRank(n int, pct float64) int {
	idx := int(float64(n)*pct/100.0+0.999999) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return idx
}
