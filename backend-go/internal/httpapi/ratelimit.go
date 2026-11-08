package httpapi

import (
	"sync"
	"time"
)

// Limiter is a small in-process token bucket keyed by client. It is a local
// abuse brake only: Vercel may run several instances, so it is NOT a global
// guarantee (plan 5.5). Idle keys are pruned to bound memory.
type Limiter struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64
	buckets map[string]*bucket
	now     func() time.Time
	lastGC  time.Time
}

type bucket struct {
	tokens float64
	seen   time.Time
}

// NewLimiter allows burst requests and refills perMinute per minute.
func NewLimiter(perMinute, burst int) *Limiter {
	return &Limiter{rate: float64(perMinute) / 60, burst: float64(burst), buckets: map[string]*bucket{}, now: time.Now}
}

// Allow consumes a token for key and returns the wait before the next one.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.lastGC) > time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.seen) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.lastGC = now
	}
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: l.burst, seen: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.seen).Seconds()*l.rate)
	b.seen = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
}
