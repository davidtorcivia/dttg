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

// clientIP returns the best-effort client IP. Behind a trusted proxy it honours
// the left-most X-Forwarded-For / X-Real-IP; otherwise it uses RemoteAddr, since
// those headers are spoofable and only meaningful when a proxy sets them.
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i >= 0 {
				xff = xff[:i]
			}
			if ip := strings.TrimSpace(xff); ip != "" {
				return ip
			}
		}
		if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
			return xr
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
