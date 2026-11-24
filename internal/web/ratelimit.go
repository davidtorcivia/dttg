package web

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

const (
	loginMaxFails       = 8                // failed attempts allowed within the window
	loginWindow         = 15 * time.Minute // rolling window for counting failures
	loginLockout        = 15 * time.Minute // lockout duration once the limit is hit
	maxRateLimitEntries = 10_000           // hard memory bound under distributed IP floods
)

type loginAttempt struct {
	fails        int
	first        time.Time
	blockedUntil time.Time
}

// loginLimiter is a bounded in-memory per-key failed-login throttle.
type loginLimiter struct {
	mu       sync.Mutex
	max      int
	attempts map[string]*loginAttempt
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{max: maxRateLimitEntries, attempts: map[string]*loginAttempt{}}
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
	if a == nil {
		if l.max > 0 && len(l.attempts) >= l.max {
			l.evictOldest()
		}
		a = &loginAttempt{first: now}
		l.attempts[key] = a
	} else if now.Sub(a.first) > loginWindow {
		a.fails = 0
		a.first = now
		a.blockedUntil = time.Time{}
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

func (l *loginLimiter) prune(now time.Time) {
	for k, a := range l.attempts {
		if now.After(a.blockedUntil) && now.Sub(a.first) > loginWindow {
			delete(l.attempts, k)
		}
	}
}

func (l *loginLimiter) evictOldest() {
	var oldestKey string
	var oldestTime time.Time
	for k, a := range l.attempts {
		if oldestKey == "" || a.first.Before(oldestTime) {
			oldestKey, oldestTime = k, a.first
		}
	}
	if oldestKey != "" {
		delete(l.attempts, oldestKey)
	}
}

// tokenBucket is a bounded per-key token-bucket limiter.
type tokenBucket struct {
	mu      sync.Mutex
	max     int
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
		max:     maxRateLimitEntries,
		rate:    rate,
		burst:   float64(burst),
		buckets: map[string]*tbEntry{},
	}
}

// allow consumes one token for key when available, else reports how long to
// wait for the next one (for Retry-After). Thread-safe and memory-bounded.
func (tb *tokenBucket) allow(key string) (bool, time.Duration) {
	now := time.Now()
	tb.mu.Lock()
	defer tb.mu.Unlock()
	tb.prune(now)

	e := tb.buckets[key]
	if e == nil {
		if tb.max > 0 && len(tb.buckets) >= tb.max {
			tb.evictOldest()
		}
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

func (tb *tokenBucket) prune(now time.Time) {
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

func (tb *tokenBucket) evictOldest() {
	var oldestKey string
	var oldestTime time.Time
	for k, e := range tb.buckets {
		if oldestKey == "" || e.last.Before(oldestTime) {
			oldestKey, oldestTime = k, e.last
		}
	}
	if oldestKey != "" {
		delete(tb.buckets, oldestKey)
	}
}

// clientIP returns the best-effort client IP. Behind a trusted proxy/tunnel it
// prefers CF-Connecting-IP (Cloudflare Tunnel's authoritative single client IP),
// then X-Real-IP, then the right-most X-Forwarded-For entry — validating IP
// syntax so malformed headers can't poison rate-limit buckets.
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if cf := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cf != "" {
			if addr, err := netip.ParseAddr(cf); err == nil {
				return addr.Unmap().String()
			}
		}
		if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
			if addr, err := netip.ParseAddr(xr); err == nil {
				return addr.Unmap().String()
			}
			return xr
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.LastIndexByte(xff, ','); i >= 0 {
				xff = xff[i+1:]
			}
			if ip := strings.TrimSpace(xff); ip != "" {
				if addr, err := netip.ParseAddr(ip); err == nil {
					return addr.Unmap().String()
				}
				return ip
			}
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
