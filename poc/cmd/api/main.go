// api es el orquestador. Recibe la request del cliente, hace fan-out a los 10
// servicios black box y responde cuando convergen las 10 respuestas.
//
// Endpoints:
//
//	GET /aggregate  fan-out con la estrategia configurada por flags
//	GET /baseline   fan-out simple (1 llamada por servicio), para comparar
//	GET /stats      contadores por endpoint y hedge delay actual
//	GET /healthz
//
// Estrategias tipicas (ver internal/fanout):
//
//	baseline       -initial 1 -copies 1
//	double         -initial 2 -copies 2
//	hedged p95     -initial 1 -copies 2 -hedge-pct 95
//	double+hedge   -initial 2 -copies 3 -hedge-pct 95
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"tailatscale/internal/fanout"
)

type counters struct {
	requests  atomic.Int64
	failed    atomic.Int64
	attempts  atomic.Int64
	hedges    atomic.Int64
	hedgeWins atomic.Int64
}

type endpoint struct {
	f *fanout.Fanout
	c counters
}

// logLine es una fila de la tabla de logs transaccionales, una por request y
// con su latencia.
type logLine struct {
	ts        time.Time
	strategy  string
	latencyMs float64
	attempts  int
	hedges    int
	hedgeWins int
	ok        bool
}

// asyncLogger escribe a CSV en una goroutine aparte. Si el buffer se llena se
// descarta la linea, por lo tanto el logging nunca frena ni tumba la API.
type asyncLogger struct {
	ch      chan logLine
	dropped atomic.Int64
	done    chan struct{}
}

func newAsyncLogger(path string) (*asyncLogger, error) {
	l := &asyncLogger{ch: make(chan logLine, 20000), done: make(chan struct{})}
	if path == "" {
		go func() {
			for range l.ch {
			}
			close(l.done)
		}()
		return l, nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	go func() {
		defer close(l.done)
		defer f.Close()
		w := bufio.NewWriter(f)
		defer w.Flush()
		fmt.Fprintln(w, "ts_unix_ms,strategy,latency_ms,attempts,hedges,hedge_wins,ok")
		flush := time.NewTicker(500 * time.Millisecond)
		defer flush.Stop()
		for {
			select {
			case ln, open := <-l.ch:
				if !open {
					return
				}
				fmt.Fprintf(w, "%d,%s,%.3f,%d,%d,%d,%t\n", ln.ts.UnixMilli(), ln.strategy,
					ln.latencyMs, ln.attempts, ln.hedges, ln.hedgeWins, ln.ok)
			case <-flush.C:
				w.Flush()
			}
		}
	}()
	return l, nil
}

func (l *asyncLogger) log(ln logLine) {
	select {
	case l.ch <- ln:
	default:
		l.dropped.Add(1)
	}
}

func (e *endpoint) handler(lg *asyncLogger, timeout time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		res := e.f.Do(ctx)

		ok := res.Errors == 0
		e.c.requests.Add(1)
		if !ok {
			e.c.failed.Add(1)
		}
		e.c.attempts.Add(int64(res.Attempts))
		e.c.hedges.Add(int64(res.Hedges))
		e.c.hedgeWins.Add(int64(res.HedgeWins))

		latMs := float64(res.Latency.Microseconds()) / 1000.0
		lg.log(logLine{time.Now(), e.f.Strategy().Name, latMs, res.Attempts, res.Hedges, res.HedgeWins, ok})

		w.Header().Set("Content-Type", "application/json")
		if !ok {
			w.WriteHeader(http.StatusBadGateway)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"strategy":   e.f.Strategy().Name,
			"latency_ms": latMs,
			"attempts":   res.Attempts,
			"hedges":     res.Hedges,
			"hedge_wins": res.HedgeWins,
			"errors":     res.Errors,
			"results":    res.Results,
		})
	}
}

func main() {
	port := flag.Int("port", 8080, "puerto de la API")
	svcHost := flag.String("services-host", "http://127.0.0.1", "host de los servicios black box")
	svcBase := flag.Int("services-base-port", 9001, "puerto de S1")
	nSvc := flag.Int("services", 10, "cantidad de servicios")
	timeout := flag.Duration("timeout", 10*time.Second, "timeout total por request")
	logPath := flag.String("log", "", "CSV de logs transaccionales (vacio = no loguear)")

	name := flag.String("name", "aggregate", "nombre de la estrategia (solo para logs y stats)")
	initial := flag.Int("initial", 1, "copias por servicio en t=0")
	copies := flag.Int("copies", 1, "maximo de copias por servicio")
	hedgePct := flag.Float64("hedge-pct", 95, "percentil observado que dispara la siguiente copia")
	hedgeFixed := flag.Duration("hedge-delay", 0, "delay fijo; si es >0 desactiva el modo adaptativo")
	minDelay := flag.Duration("hedge-min", 2*time.Millisecond, "delay minimo del hedge")
	maxDelay := flag.Duration("hedge-max", 250*time.Millisecond, "delay maximo del hedge")
	warmDelay := flag.Duration("hedge-warmup", 30*time.Millisecond, "delay mientras no hay muestras")
	budget := flag.Float64("budget", 2.0, "tokens de hedge por llamada primaria (0.1 = +10%, 2.0 = carga max 3x; <=0 sin limite)")
	burst := flag.Float64("budget-burst", 200, "tokens maximos acumulables")
	drain := flag.Bool("drain", false, "deja terminar las copias perdedoras en vez de cancelarlas (no cierra conexiones)")
	actions := flag.String("actions", "S10", "servicios accion (coma), nunca se duplican; vacio = todos lectura")
	flag.Parse()

	isAction := map[string]bool{}
	for _, a := range strings.Split(*actions, ",") {
		if a = strings.TrimSpace(a); a != "" {
			isAction[a] = true
		}
	}
	var services []fanout.Service
	for i := 0; i < *nSvc; i++ {
		n := fmt.Sprintf("S%d", i+1)
		services = append(services, fanout.Service{
			Name:   n,
			URL:    fmt.Sprintf("%s:%d/", *svcHost, *svcBase+i),
			Action: isAction[n],
		})
	}

	client := &http.Client{Transport: &http.Transport{
		MaxIdleConns:        4000,
		MaxIdleConnsPerHost: 1000,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}}

	endpoints := map[string]*endpoint{
		"baseline": {f: fanout.New(client, services, fanout.Strategy{Name: "baseline", MaxCopies: 1})},
		"aggregate": {f: fanout.New(client, services, fanout.Strategy{
			Name:          *name,
			InitialCopies: *initial,
			MaxCopies:     *copies,
			Adaptive:      *hedgeFixed <= 0,
			Percentile:    *hedgePct,
			FixedDelay:    *hedgeFixed,
			MinDelay:      *minDelay,
			MaxDelay:      *maxDelay,
			WarmupDelay:   *warmDelay,
			Budget:        fanout.NewBudget(*budget, *burst),
			DrainLosers:   *drain,
		})},
	}

	lg, err := newAsyncLogger(*logPath)
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	for name, e := range endpoints {
		mux.HandleFunc("/"+name, e.handler(lg, *timeout))
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
		out := map[string]any{"log_dropped": lg.dropped.Load()}
		for name, e := range endpoints {
			out[name] = map[string]any{
				"strategy":   e.f.Strategy().Name,
				"requests":   e.c.requests.Load(),
				"failed":     e.c.failed.Load(),
				"attempts":   e.c.attempts.Load(),
				"hedges":     e.c.hedges.Load(),
				"hedge_wins": e.c.hedgeWins.Load(),
			}
		}
		out["hedge_delay_ms"] = endpoints["aggregate"].f.HedgeDelays()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})

	srv := &http.Server{Addr: fmt.Sprintf(":%d", *port), Handler: mux}
	log.Printf("api en :%d  %s: initial=%d copies=%d pct=%.0f budget=%.2f actions=%q",
		*port, *name, *initial, *copies, *hedgePct, *budget, *actions)
	log.Fatal(srv.ListenAndServe())
}
