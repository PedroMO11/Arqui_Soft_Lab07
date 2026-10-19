// blackbox levanta los 10 servicios externos simulados (S1..S10), uno por
// puerto. La API los trata como cajas negras y solo hace GET esperando JSON.
//
// Escenarios de latencia (-scenario):
//
//	request: cada llamada tiene una probabilidad pSlow de ser lenta, de forma
//	         independiente (cola interna, GC, backend ocupado). S7, S8 y S9 son
//	         los servicios "flaky" del enunciado.
//	episode: los servicios flaky entran en episodios donde TODAS sus llamadas
//	         son lentas por ~1.5 s (lentitud correlacionada del servicio).
//	mixed:   ambos a la vez.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type profile struct {
	name    string
	baseMin float64 // ms
	baseMax float64
	pSlow   float64
	slowMin float64
	slowMax float64
	flaky   bool
}

// specProfiles sigue los supuestos del README del equipo. Todos los servicios
// responden en 10 a 30 ms y el 2% de las llamadas tarda 1 a 1.5 s, de forma
// independiente. En los escenarios con episodios, S7 y S8 son los que se degradan.
func specProfiles() []profile {
	var out []profile
	for i := 1; i <= 10; i++ {
		out = append(out, profile{fmt.Sprintf("S%d", i), 10, 30, 0.02, 1000, 1500, i == 7 || i == 8})
	}
	return out
}

// flakyProfiles concentra la lentitud en S7, S8 y S9 (4%) y deja el resto al 0.3%.
var flakyProfiles = []profile{
	{"S1", 8, 14, 0.003, 300, 800, false},
	{"S2", 9, 15, 0.003, 300, 800, false},
	{"S3", 8, 13, 0.003, 300, 800, false},
	{"S4", 10, 16, 0.003, 300, 800, false},
	{"S5", 8, 14, 0.003, 300, 800, false},
	{"S6", 9, 15, 0.003, 300, 800, false},
	{"S7", 10, 18, 0.04, 900, 1100, true},
	{"S8", 10, 18, 0.04, 1300, 1600, true},
	{"S9", 9, 16, 0.04, 700, 1200, true},
	{"S10", 8, 14, 0.003, 300, 800, false},
}

type service struct {
	p        profile
	scenario string

	mu  sync.Mutex
	rng *rand.Rand

	episodeUntil atomic.Int64 // unix nanos
	sem          chan struct{}

	received  atomic.Int64
	served    atomic.Int64
	cancelled atomic.Int64
	slow      atomic.Int64
}

func (s *service) uniform(a, b float64) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return a + s.rng.Float64()*(b-a)
}

func (s *service) chance(p float64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rng.Float64() < p
}

func (s *service) sample() (time.Duration, bool) {
	ms := s.uniform(s.p.baseMin, s.p.baseMax)
	slow := false
	perRequest := s.scenario == "request" || s.scenario == "mixed"
	inEpisode := time.Now().UnixNano() < s.episodeUntil.Load()
	switch {
	case inEpisode:
		ms = s.uniform(s.p.slowMin, s.p.slowMax)
		slow = true
	case perRequest && s.chance(s.p.pSlow):
		ms = s.uniform(s.p.slowMin, s.p.slowMax)
		slow = true
	}
	return time.Duration(ms * float64(time.Millisecond)), slow
}

// episodes programa los episodios de lentitud con el rng sembrado, asi
// baseline y propuesta ven el mismo calendario de episodios.
func (s *service) episodes(meanGap, length time.Duration) {
	s.mu.Lock()
	r := rand.New(rand.NewPCG(s.rng.Uint64(), 7))
	s.mu.Unlock()
	for {
		gap := time.Duration(-math.Log(1-r.Float64()) * float64(meanGap))
		time.Sleep(gap)
		s.episodeUntil.Store(time.Now().Add(length).UnixNano())
		time.Sleep(length)
	}
}

func (s *service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.received.Add(1)
	if s.sem != nil {
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		case <-r.Context().Done():
			s.cancelled.Add(1)
			return
		}
	}
	d, slow := s.sample()
	if slow {
		s.slow.Add(1)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-r.Context().Done():
		s.cancelled.Add(1)
		return
	}
	s.served.Add(1)
	w.Header().Set("Content-Type", "application/json")
	s.mu.Lock()
	value := s.rng.IntN(1000)
	s.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]any{
		"service":    s.p.name,
		"value":      value,
		"latency_ms": float64(d.Microseconds()) / 1000.0,
	})
}

func main() {
	basePort := flag.Int("base-port", 9001, "puerto de S1; S2 usa base+1, etc.")
	adminPort := flag.Int("admin-port", 9000, "puerto de /stats y /healthz")
	scenario := flag.String("scenario", "request", "request | episode | mixed")
	profileName := flag.String("profile", "spec", "spec (2% en todos) | flaky (S7, S8, S9 al 4%)")
	seed := flag.Uint64("seed", 42, "semilla del generador de latencias")
	workers := flag.Int("workers", 0, "requests concurrentes por servicio (0 = sin limite)")
	meanGap := flag.Duration("episode-gap", 20*time.Second, "tiempo medio entre episodios (episode/mixed)")
	epLen := flag.Duration("episode-len", 1500*time.Millisecond, "duracion de cada episodio")
	flag.Parse()

	switch *scenario {
	case "request", "episode", "mixed":
	default:
		log.Fatalf("scenario invalido: %s", *scenario)
	}

	profiles := specProfiles()
	switch *profileName {
	case "spec":
	case "flaky":
		profiles = flakyProfiles
	default:
		log.Fatalf("profile invalido: %s", *profileName)
	}

	var services []*service
	for i, p := range profiles {
		s := &service{p: p, scenario: *scenario, rng: rand.New(rand.NewPCG(*seed, uint64(i+1)))}
		if *workers > 0 {
			s.sem = make(chan struct{}, *workers)
		}
		services = append(services, s)
		if p.flaky && *scenario != "request" {
			go s.episodes(*meanGap, *epLen)
		}
		addr := fmt.Sprintf(":%d", *basePort+i)
		go func(addr string, s *service) {
			log.Fatal(http.ListenAndServe(addr, s))
		}(addr, s)
	}

	admin := http.NewServeMux()
	admin.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	admin.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
		out := map[string]any{}
		var total int64
		for _, s := range services {
			rec := s.received.Load()
			total += rec
			out[s.p.name] = map[string]int64{
				"received":  rec,
				"served":    s.served.Load(),
				"cancelled": s.cancelled.Load(),
				"slow":      s.slow.Load(),
			}
		}
		out["total_received"] = total
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})
	log.Printf("blackbox: profile=%s scenario=%s seed=%d S1..S10 en :%d..:%d", *profileName, *scenario, *seed, *basePort, *basePort+9)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *adminPort), admin))
}
