package web

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	sessionCookie = "dnttg_session"
	sessionTTL    = 30 * 24 * time.Hour
	pbkdf2Iter    = 600_000
)

// HashPassword derives a salted PBKDF2-SHA256 hash, encoded as
// "pbkdf2_sha256$iter$salt$hash" (all stdlib, no cgo).
func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding.EncodeToString
	return fmt.Sprintf("pbkdf2_sha256$%d$%s$%s", pbkdf2Iter, enc(salt), enc(dk)), nil
}

func VerifyPassword(pw, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2_sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// NewToken returns a high-entropy bearer token (shown to the user once).
func NewToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashToken stores tokens as their SHA-256 (tokens are already high-entropy).
func HashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// HashSession stores session IDs as their SHA-256, same as API tokens. The
// browser cookie still carries the raw random ID; only the hash is persisted.
func HashSession(id string) string { return HashToken(id) }

func newSessionID() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// isAdmin reports whether the request carries a valid session, memoized per
// request by withAdmin when present.
func (s *Server) isAdmin(r *http.Request) bool {
	if a, ok := r.Context().Value(adminKey{}).(*adminOnce); ok {
		a.once.Do(func() { a.ok = s.lookupAdmin(r) })
		return a.ok
	}
	return s.lookupAdmin(r)
}

func (s *Server) lookupAdmin(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	ok, _ := s.store.SessionValid(r.Context(), HashSession(c.Value))
	return ok
}

// setSessionCookie sets the session cookie; maxAge < 0 clears it.
func (s *Server) setSessionCookie(w http.ResponseWriter, id string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.HTTPSBase(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	})
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if s.isAdmin(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	pd := s.page(r, "LOGIN")
	pd.Next = r.URL.Query().Get("next")
	s.render(w, http.StatusOK, "login.html", pd)
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	pd := s.page(r, "LOGIN")
	pd.Next = r.FormValue("next")
	if blocked, retry := s.loginRL.blocked(ip); blocked {
		pd.Error = "Too many attempts. Try again later."
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		s.render(w, http.StatusTooManyRequests, "login.html", pd)
		return
	}
	hash, _ := s.store.GetSetting(r.Context(), "password_hash")
	switch {
	case hash == "":
		pd.Error = "No password configured. Run: dnttg set-password"
		s.render(w, http.StatusServiceUnavailable, "login.html", pd)
	case !VerifyPassword(r.FormValue("password"), hash):
		s.loginRL.fail(ip)
		time.Sleep(400 * time.Millisecond) // throttle brute force
		pd.Error = "Incorrect password."
		s.render(w, http.StatusUnauthorized, "login.html", pd)
	default:
		s.loginRL.reset(ip)
		sid := newSessionID()
		if err := s.store.CreateSession(r.Context(), HashSession(sid), sessionTTL); err != nil {
			s.serverError(w, r, err)
			return
		}
		s.setSessionCookie(w, sid, int(sessionTTL.Seconds()))
		http.Redirect(w, r, safeNext(pd.Next), http.StatusSeeOther)
	}
}

// safeNext returns dest when it is a same-origin relative path; otherwise "/".
// Browsers treat "\" like "/", so "/\evil.com" would be protocol-relative.
func safeNext(dest string) string {
	if !strings.HasPrefix(dest, "/") || strings.HasPrefix(dest, "//") || strings.ContainsAny(dest, "\\\r\n") {
		return "/"
	}
	return dest
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.store.DeleteSession(r.Context(), HashSession(c.Value))
	}
	s.setSessionCookie(w, "", -1)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
