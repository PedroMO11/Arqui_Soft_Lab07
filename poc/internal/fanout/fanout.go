// Package fanout implementa el fan-out de la API hacia los 10 servicios
// black box. Todas las estrategias comparten el mismo codigo y se definen con
// tres parametros por servicio:
//
//   - InitialCopies: copias que salen en t=0 (1 = normal, 2 = double).
//   - MaxCopies:     tope de copias por servicio.
//   - delay:         cuando sale la siguiente copia (pXX observado o fijo).
//
// Ejemplos: baseline (1,1), double (2,2), triple (3,3), hedged p95 (1,2 o 1,3),
// double+hedge (2,3). Gana la primera respuesta y el resto se cancela.
// Los servicios marcados como accion nunca se duplican, porque no se sabe si
// son idempotentes. Los servicios no se modifican, todo pasa del lado de la API.
package fanout

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

type Service struct {
	Name   string
	URL    string
	Action bool // accion no idempotente, siempre 1 sola llamada
}

type Strategy struct {
	Name          string
	InitialCopies int           // copias en t=0, sin consumir budget (default 1)
	MaxCopies     int           // tope de copias por servicio (1 = baseline)
	Adaptive      bool          // delay = percentil observado por servicio
	Percentile    float64       // ej. 95
	FixedDelay    time.Duration // si !Adaptive; 0 = todas las copias en t=0 (naive)
	MinDelay      time.Duration
	MaxDelay      time.Duration
	WarmupDelay   time.Duration // delay mientras el tracker no tiene muestras
	Budget        *Budget
	// DrainLosers deja terminar las copias perdedoras en background en vez de
	// cancelarlas. En HTTP/1.1 cancelar una request cierra su conexion TCP, por
	// lo tanto con muchas copias el costo real es abrir conexiones nuevas.
	DrainLosers bool
}

// drainTimeout acota cuanto puede vivir una copia perdedora en modo drain.
const drainTimeout = 10 * time.Second

type Fanout struct {
	client   *http.Client
	services []Service
	strategy Strategy
	trackers []*Tracker
}

func New(client *http.Client, services []Service, s Strategy) *Fanout {
	if s.MaxCopies < 1 {
		s.MaxCopies = 1
	}
	f := &Fanout{client: client, services: services, strategy: s}
	for range services {
		f.trackers = append(f.trackers, NewTracker(s.Percentile))
	}
	return f
}

func (f *Fanout) Strategy() Strategy { return f.strategy }

// RequestResult es lo que la API devuelve y loguea por cada request de usuario.
type RequestResult struct {
	Latency   time.Duration
	Attempts  int // llamadas HTTP emitidas hacia los servicios
	Hedges    int // copias extra emitidas
	HedgeWins int // servicios donde gano una copia extra
	Errors    int
	Results   []json.RawMessage
}

type attempt struct {
	body    []byte
	err     error
	latency time.Duration
	copyIdx int
}

type callResult struct {
	body     []byte
	err      error
	attempts int
	hedgeWon bool
}

// Do hace el fan-out a los 10 servicios en paralelo y espera las 10
// respuestas, por lo tanto la respuesta final siempre converge completa.
func (f *Fanout) Do(ctx context.Context) RequestResult {
	start := time.Now()
	out := make([]callResult, len(f.services))
	var wg sync.WaitGroup
	for i := range f.services {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out[i] = f.callService(ctx, i)
		}(i)
	}
	wg.Wait()

	res := RequestResult{Results: make([]json.RawMessage, 0, len(out))}
	for _, c := range out {
		res.Attempts += c.attempts
		res.Hedges += c.attempts - 1
		if c.hedgeWon {
			res.HedgeWins++
		}
		if c.err != nil {
			res.Errors++
			continue
		}
		res.Results = append(res.Results, c.body)
	}
	res.Latency = time.Since(start)
	return res
}

func (f *Fanout) callService(parent context.Context, i int) callResult {
	s := f.strategy
	maxCopies, initial := s.MaxCopies, s.InitialCopies
	if initial < 1 {
		initial = 1
	}
	if initial > maxCopies {
		maxCopies = initial
	}
	if f.services[i].Action {
		maxCopies, initial = 1, 1
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel() // cancela las copias perdedoras

	ch := make(chan attempt, maxCopies) // con buffer las perdedoras nunca se bloquean
	launched := 0
	launch := func() {
		idx := launched
		launched++
		go func() {
			t0 := time.Now()
			reqCtx := ctx
			if s.DrainLosers {
				var c context.CancelFunc
				reqCtx, c = context.WithTimeout(context.WithoutCancel(parent), drainTimeout)
				defer c()
			}
			body, err := f.get(reqCtx, f.services[i].URL)
			ch <- attempt{body: body, err: err, latency: time.Since(t0), copyIdx: idx}
		}()
	}

	for launched < initial {
		launch()
	}
	if maxCopies > initial {
		s.Budget.Deposit()
	}

	delay := f.hedgeDelay(i)
	var timer *time.Timer
	var timerC <-chan time.Time
	if maxCopies > launched {
		if delay <= 0 {
			for launched < maxCopies && s.Budget.TryAcquire() {
				launch()
			}
		} else {
			timer = time.NewTimer(delay)
			defer timer.Stop()
			timerC = timer.C
		}
	}

	outstanding := launched
	var lastErr error
	for {
		select {
		case a := <-ch:
			outstanding--
			if a.err == nil {
				if s.Adaptive {
					f.trackers[i].Add(a.latency)
				}
				return callResult{body: a.body, attempts: launched, hedgeWon: a.copyIdx >= initial}
			}
			lastErr = a.err
			if outstanding == 0 {
				// si todas fallaron y queda cupo, se lanza otra copia de inmediato
				if launched < maxCopies && s.Budget.TryAcquire() {
					launch()
					outstanding++
					continue
				}
				return callResult{err: lastErr, attempts: launched}
			}
		case <-timerC:
			before := launched
			if launched < maxCopies && s.Budget.TryAcquire() {
				launch()
				outstanding += launched - before
			}
			if launched < maxCopies {
				timer.Reset(delay)
			} else {
				timerC = nil
			}
		case <-parent.Done():
			return callResult{err: parent.Err(), attempts: launched}
		}
	}
}

func (f *Fanout) hedgeDelay(i int) time.Duration {
	s := f.strategy
	if !s.Adaptive {
		return s.FixedDelay
	}
	d, ok := f.trackers[i].Percentile()
	if !ok {
		d = s.WarmupDelay
	}
	if s.MinDelay > 0 && d < s.MinDelay {
		d = s.MinDelay
	}
	if s.MaxDelay > 0 && d > s.MaxDelay {
		d = s.MaxDelay
	}
	return d
}

func (f *Fanout) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return body, nil
}

// HedgeDelays expone el delay actual por servicio (para /stats).
func (f *Fanout) HedgeDelays() map[string]float64 {
	out := map[string]float64{}
	for i, svc := range f.services {
		out[svc.Name] = float64(f.hedgeDelay(i).Microseconds()) / 1000.0
	}
	return out
}
