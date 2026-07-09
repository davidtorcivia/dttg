package web

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	loginMaxFails = 8                // failed attempts allowed within the window
	loginWindow   = 15 * time.Minute // rolling window for counting failures
	loginLockout  = 15 * time.Minute // lockout duration once the limit is hit
)

type loginAttempt struct {
	fails        int
	first        time.Time
	blockedUntil time.Time
}

// loginLimiter is a tiny in-memory per-key failed-login throttle. A legitimate
// admin succeeds on the first try and is never counted; bots get locked out after
// loginMaxFails failures within loginWindow. Keyed by client IP.
type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string]*loginAttempt
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{attempts: map[string]*loginAttempt{}}
}

// blocked reports whether key is currently locked out, and the remaining time.
func (l *loginLimiter) blocked(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.attempts[key]
	if a == nil {
		return false, 0
	}
	if d := time.Until(a.blockedUntil); d > 0 {
		return true, d
	}
	return false, 0
}

// fail records a failed attempt, locking the key out once the limit is reached.
func (l *loginLimiter) fail(key string) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(now)
	a := l.attempts[key]
	if a == nil || now.Sub(a.first) > loginWindow {
		a = &loginAttempt{first: now}
		l.attempts[key] = a
	}
	a.fails++
	if a.fails >= loginMaxFails {
		a.blockedUntil = now.Add(loginLockout)
	}
}

// reset clears a key's record (call on a successful login).
func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, key)
}

// prune drops stale records so the map can't grow without bound (lock held).
func (l *loginLimiter) prune(now time.Time) {
	for k, a := range l.attempts {
		if now.After(a.blockedUntil) && now.Sub(a.first) > loginWindow {
			delete(l.attempts, k)
		}
	}
}

// tokenBucket is a per-key token-bucket limiter. rate is tokens refilled per
// second; burst is the maximum accumulated tokens (and thus the short spike
// size). Allow returns false when the key has exhausted its budget.
type tokenBucket struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64
	buckets map[string]*tbEntry
}

type tbEntry struct {
	tokens float64
	last   time.Time
}

func newTokenBucket(rate float64, burst int) *tokenBucket {
	if rate <= 0 {
		rate = 1
	}
	if burst < 1 {
		burst = 1
	}
	return &tokenBucket{
		rate:    rate,
		burst:   float64(burst),
		buckets: map[string]*tbEntry{},
	}
}

// Allow consumes one token for key when available. Thread-safe.
func (tb *tokenBucket) Allow(key string) bool {
	ok, _ := tb.allow(key)
	return ok
}

// allow is like Allow but also returns how long to wait for the next token when
// denied (for Retry-After).
func (tb *tokenBucket) allow(key string) (bool, time.Duration) {
	now := time.Now()
	tb.mu.Lock()
	defer tb.mu.Unlock()
	tb.prune(now)

	e := tb.buckets[key]
	if e == nil {
		e = &tbEntry{tokens: tb.burst, last: now}
		tb.buckets[key] = e
	} else {
		elapsed := now.Sub(e.last).Seconds()
		if elapsed > 0 {
			e.tokens += elapsed * tb.rate
			if e.tokens > tb.burst {
				e.tokens = tb.burst
			}
			e.last = now
		}
	}
	if e.tokens < 1 {
		need := 1 - e.tokens
		retry := time.Duration(need/tb.rate*float64(time.Second)) + time.Second
		if retry < time.Second {
			retry = time.Second
		}
		return false, retry
	}
	e.tokens--
	return true, 0
}

// prune drops idle buckets (lock held). Idle = full tokens for > 2× fill time.
func (tb *tokenBucket) prune(now time.Time) {
	// Fill time to go empty→burst; keep a little extra headroom.
	idle := time.Duration(tb.burst/tb.rate*2*float64(time.Second)) + time.Minute
	if idle < 2*time.Minute {
		idle = 2 * time.Minute
	}
	for k, e := range tb.buckets {
		if now.Sub(e.last) > idle {
			delete(tb.buckets, k)
		}
	}
}

// clientIP returns the best-effort client IP. Behind a trusted proxy it prefers
// X-Real-IP (typically set by the edge to the connecting client). If only
// X-Forwarded-For is present, the right-most entry is used — the immediate
// client of the trusted proxy after the proxy has stripped inbound XFF.
// Without TrustProxy, RemoteAddr is used; those headers are spoofable.
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
			return xr
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// Right-most hop is the client adjacent to the trusted proxy.
			if i := strings.LastIndexByte(xff, ','); i >= 0 {
				xff = xff[i+1:]
			}
			if ip := strings.TrimSpace(xff); ip != "" {
				return ip
			}
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
