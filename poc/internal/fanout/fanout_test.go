package fanout

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// slowFirst responde lento a la primera llamada y rapido a las demas, lo que
// simula una lentitud por request dentro del black box.
func slowFirst(t *testing.T, slow time.Duration) (*httptest.Server, *atomic.Int64) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			select {
			case <-time.After(slow):
			case <-r.Context().Done():
				return
			}
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func TestBaselineWaitsForSlowest(t *testing.T) {
	srv, _ := slowFirst(t, 300*time.Millisecond)
	f := New(http.DefaultClient, []Service{{Name: "S1", URL: srv.URL}}, Strategy{Name: "baseline", MaxCopies: 1})
	res := f.Do(context.Background())
	if res.Latency < 300*time.Millisecond {
		t.Fatalf("baseline deberia esperar a la llamada lenta, tardo %v", res.Latency)
	}
	if res.Attempts != 1 || res.Hedges != 0 {
		t.Fatalf("baseline: attempts=%d hedges=%d", res.Attempts, res.Hedges)
	}
}

func TestHedgeWinsOverSlowPrimary(t *testing.T) {
	srv, _ := slowFirst(t, 2*time.Second)
	f := New(http.DefaultClient, []Service{{Name: "S1", URL: srv.URL}}, Strategy{
		Name: "hedged", MaxCopies: 2, FixedDelay: 20 * time.Millisecond, Budget: NewBudget(1, 10),
	})
	res := f.Do(context.Background())
	if res.Latency > 500*time.Millisecond {
		t.Fatalf("el hedge deberia ganar, tardo %v", res.Latency)
	}
	if res.Hedges != 1 || res.HedgeWins != 1 || len(res.Results) != 1 {
		t.Fatalf("hedges=%d wins=%d results=%d", res.Hedges, res.HedgeWins, len(res.Results))
	}
}

func TestNoHedgeWhenPrimaryIsFast(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	f := New(http.DefaultClient, []Service{{Name: "S1", URL: srv.URL}}, Strategy{
		Name: "hedged", MaxCopies: 3, FixedDelay: 200 * time.Millisecond, Budget: NewBudget(2, 10),
	})
	res := f.Do(context.Background())
	if res.Hedges != 0 {
		t.Fatalf("no deberia mandar hedges si la primaria responde rapido, hedges=%d", res.Hedges)
	}
}

func TestNaiveSendsAllCopies(t *testing.T) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	f := New(http.DefaultClient, []Service{{Name: "S1", URL: srv.URL}}, Strategy{Name: "naive", MaxCopies: 3})
	res := f.Do(context.Background())
	if res.Attempts != 3 {
		t.Fatalf("naive x3 deberia mandar 3 copias, mando %d", res.Attempts)
	}
}

func TestBudgetCapsHedges(t *testing.T) {
	b := NewBudget(0.5, 1)
	if !b.TryAcquire() {
		t.Fatal("el burst inicial deberia permitir 1 hedge")
	}
	if b.TryAcquire() {
		t.Fatal("sin tokens no deberia permitir hedge")
	}
	b.Deposit()
	if b.TryAcquire() {
		t.Fatal("0.5 tokens no alcanzan para un hedge")
	}
	b.Deposit()
	if !b.TryAcquire() {
		t.Fatal("2 depositos de 0.5 deberian permitir 1 hedge")
	}
}

func TestTrackerPercentile(t *testing.T) {
	tr := NewTracker(95)
	for i := 1; i <= 100; i++ {
		tr.Add(time.Duration(i) * time.Millisecond)
	}
	p, ok := tr.Percentile()
	if !ok || p != 95*time.Millisecond {
		t.Fatalf("p95 esperado 95ms, obtuvo %v ok=%v", p, ok)
	}
}

func TestActionIsNeverDuplicated(t *testing.T) {
	srv, n := slowFirst(t, 300*time.Millisecond)
	svc := []Service{{Name: "S10", URL: srv.URL, Action: true}}
	for _, s := range []Strategy{
		{Name: "triple", InitialCopies: 3, MaxCopies: 3},
		{Name: "hedged", MaxCopies: 3, FixedDelay: 10 * time.Millisecond, Budget: NewBudget(2, 10)},
	} {
		n.Store(0)
		res := New(http.DefaultClient, svc, s).Do(context.Background())
		if res.Attempts != 1 || n.Load() != 1 {
			t.Fatalf("%s: una accion debe llamarse 1 vez, attempts=%d server=%d", s.Name, res.Attempts, n.Load())
		}
	}
}

func TestDoublePlusHedgeLaunchesThirdCopyOnlyWhenBothSlow(t *testing.T) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) <= 2 { // las 2 copias iniciales son lentas
			select {
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
				return
			}
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	f := New(http.DefaultClient, []Service{{Name: "S1", URL: srv.URL}}, Strategy{
		Name: "double+hedge", InitialCopies: 2, MaxCopies: 3, FixedDelay: 20 * time.Millisecond, Budget: NewBudget(2, 10),
	})
	res := f.Do(context.Background())
	if res.Attempts != 3 || res.HedgeWins != 1 || res.Latency > 500*time.Millisecond {
		t.Fatalf("attempts=%d wins=%d latency=%v", res.Attempts, res.HedgeWins, res.Latency)
	}
}
