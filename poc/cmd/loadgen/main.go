// loadgen es un generador de carga open-loop. Dispara requests a una tasa
// fija sin esperar a que terminen las anteriores y mide la latencia desde
// el instante en que la request DEBIA salir. Asi se evita el "coordinated
// omission" de los generadores closed-loop, que esconde justo la cola.
package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
)

type sample struct {
	offset    time.Duration
	latency   time.Duration
	ok        bool
	attempts  int
	hedges    int
	hedgeWins int
}

type apiResp struct {
	Attempts  int `json:"attempts"`
	Hedges    int `json:"hedges"`
	HedgeWins int `json:"hedge_wins"`
}

type Summary struct {
	Label            string  `json:"label"`
	URL              string  `json:"url"`
	TargetRPS        float64 `json:"target_rps"`
	AchievedRPS      float64 `json:"achieved_rps"`
	Requests         int     `json:"requests"`
	OK               int     `json:"ok"`
	Errors           int     `json:"errors"`
	P50              float64 `json:"p50_ms"`
	P90              float64 `json:"p90_ms"`
	P95              float64 `json:"p95_ms"`
	P99              float64 `json:"p99_ms"`
	P999             float64 `json:"p999_ms"`
	Max              float64 `json:"max_ms"`
	Mean             float64 `json:"mean_ms"`
	DownstreamCalls  int     `json:"downstream_calls"`
	Hedges           int     `json:"hedges"`
	HedgeWins        int     `json:"hedge_wins"`
	CallsPerRequest  float64 `json:"calls_per_request"`
	LoadMultiplier   float64 `json:"load_multiplier"`
	SlowOver100msPct float64 `json:"slow_over_100ms_pct"`
}

func main() {
	url := flag.String("url", "http://127.0.0.1:8080/baseline", "endpoint a probar")
	rps := flag.Float64("rps", 100, "requests por segundo (open-loop)")
	duration := flag.Duration("duration", 60*time.Second, "duracion medida")
	warmup := flag.Duration("warmup", 5*time.Second, "warm-up (no se cuenta)")
	fanoutN := flag.Int("fanout", 10, "servicios por request (para calcular la carga xN)")
	out := flag.String("out", "", "CSV con una fila por request")
	summaryPath := flag.String("summary", "", "JSON con el resumen")
	label := flag.String("label", "", "nombre de la corrida")
	flag.Parse()

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        4000,
			MaxIdleConnsPerHost: 4000,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	interval := time.Duration(float64(time.Second) / *rps)
	total := *warmup + *duration
	var (
		mu      sync.Mutex
		samples []sample
		wg      sync.WaitGroup
	)
	start := time.Now()
	for i := 0; ; i++ {
		intended := start.Add(time.Duration(i) * interval)
		if intended.Sub(start) >= total {
			break
		}
		if d := time.Until(intended); d > 0 {
			time.Sleep(d)
		}
		wg.Add(1)
		go func(intended time.Time) {
			defer wg.Done()
			s := sample{offset: intended.Sub(start)}
			resp, err := client.Get(*url)
			if err == nil {
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				var ar apiResp
				_ = json.Unmarshal(body, &ar)
				s.ok = resp.StatusCode == http.StatusOK
				s.attempts, s.hedges, s.hedgeWins = ar.Attempts, ar.Hedges, ar.HedgeWins
			}
			s.latency = time.Since(intended)
			mu.Lock()
			samples = append(samples, s)
			mu.Unlock()
		}(intended)
	}
	wg.Wait()

	sort.Slice(samples, func(i, j int) bool { return samples[i].offset < samples[j].offset })

	if *out != "" {
		if err := writeCSV(*out, samples); err != nil {
			log.Fatal(err)
		}
	}

	var measured []sample
	for _, s := range samples {
		if s.offset >= *warmup {
			measured = append(measured, s)
		}
	}
	sum := summarize(measured, *duration, *fanoutN)
	sum.Label, sum.URL, sum.TargetRPS = *label, *url, *rps

	b, _ := json.MarshalIndent(sum, "", "  ")
	fmt.Println(string(b))
	if *summaryPath != "" {
		if err := os.WriteFile(*summaryPath, b, 0o644); err != nil {
			log.Fatal(err)
		}
	}
}

func summarize(ss []sample, d time.Duration, fanoutN int) Summary {
	var s Summary
	s.Requests = len(ss)
	if len(ss) == 0 {
		return s
	}
	lat := make([]float64, 0, len(ss))
	var sumMs float64
	slow := 0
	for _, x := range ss {
		ms := float64(x.latency.Microseconds()) / 1000.0
		lat = append(lat, ms)
		sumMs += ms
		if ms > 100 {
			slow++
		}
		if x.ok {
			s.OK++
		} else {
			s.Errors++
		}
		s.DownstreamCalls += x.attempts
		s.Hedges += x.hedges
		s.HedgeWins += x.hedgeWins
	}
	sort.Float64s(lat)
	s.P50, s.P90, s.P95 = pct(lat, 50), pct(lat, 90), pct(lat, 95)
	s.P99, s.P999 = pct(lat, 99), pct(lat, 99.9)
	s.Max = lat[len(lat)-1]
	s.Mean = round(sumMs / float64(len(lat)))
	s.AchievedRPS = round(float64(len(ss)) / d.Seconds())
	s.CallsPerRequest = round(float64(s.DownstreamCalls) / float64(len(ss)))
	s.LoadMultiplier = round(s.CallsPerRequest / float64(fanoutN))
	s.SlowOver100msPct = round(100 * float64(slow) / float64(len(ss)))
	return s
}

// pct usa nearest-rank sobre datos ya ordenados.
func pct(sorted []float64, p float64) float64 {
	idx := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return round(sorted[idx])
}

func round(x float64) float64 { return math.Round(x*100) / 100 }

func writeCSV(path string, ss []sample) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	w.Write([]string{"offset_ms", "latency_ms", "ok", "attempts", "hedges", "hedge_wins"})
	for _, s := range ss {
		w.Write([]string{
			strconv.FormatInt(s.offset.Milliseconds(), 10),
			strconv.FormatFloat(float64(s.latency.Microseconds())/1000.0, 'f', 3, 64),
			strconv.FormatBool(s.ok),
			strconv.Itoa(s.attempts),
			strconv.Itoa(s.hedges),
			strconv.Itoa(s.hedgeWins),
		})
	}
	return nil
}
