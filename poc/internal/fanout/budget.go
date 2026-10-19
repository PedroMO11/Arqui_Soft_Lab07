package fanout

import "sync"

// Budget es un token bucket que limita cuantas copias extra (hedges) se
// pueden mandar. Cada llamada primaria deposita `ratio` tokens y cada hedge
// consume 1. Con ratio=2.0 la carga hacia los servicios nunca pasa de 3x,
// que es el maximo que acepta el enunciado. ratio<=0 desactiva el limite.
type Budget struct {
	mu     sync.Mutex
	tokens float64
	ratio  float64
	max    float64
}

func NewBudget(ratio, burst float64) *Budget {
	return &Budget{tokens: burst, ratio: ratio, max: burst}
}

func (b *Budget) Unlimited() bool { return b == nil || b.ratio <= 0 }

func (b *Budget) Deposit() {
	if b.Unlimited() {
		return
	}
	b.mu.Lock()
	b.tokens += b.ratio
	if b.tokens > b.max {
		b.tokens = b.max
	}
	b.mu.Unlock()
}

func (b *Budget) TryAcquire() bool {
	if b.Unlimited() {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}
