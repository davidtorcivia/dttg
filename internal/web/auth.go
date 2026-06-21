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

func newSessionID() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Server) isAdmin(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	ok, _ := s.store.SessionValid(r.Context(), c.Value)
	return ok
}

func (s *Server) setSessionCookie(w http.ResponseWriter, id string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.HTTPSBase(),
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(sessionTTL),
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.HTTPSBase(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if s.isAdmin(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, "login.html", s.page(r, "LOGIN"))
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if blocked, retry := s.loginRL.blocked(ip); blocked {
		pd := s.page(r, "LOGIN")
		pd.Error = "Too many attempts. Try again later."
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		w.WriteHeader(http.StatusTooManyRequests)
		s.render(w, "login.html", pd)
		return
	}
	pw := r.FormValue("password")
	hash, _ := s.store.GetSetting(r.Context(), "password_hash")
	pd := s.page(r, "LOGIN")
	switch {
	case hash == "":
		pd.Error = "No password configured. Run: dnttg set-password <password>"
		s.render(w, "login.html", pd)
	case !VerifyPassword(pw, hash):
		s.loginRL.fail(ip)
		time.Sleep(400 * time.Millisecond) // throttle brute force
		pd.Error = "Incorrect password."
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, "login.html", pd)
	default:
		s.loginRL.reset(ip)
		sid := newSessionID()
		if err := s.store.CreateSession(r.Context(), sid, sessionTTL); err != nil {
			s.serverError(w, r, err)
			return
		}
		s.setSessionCookie(w, sid)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.store.DeleteSession(r.Context(), c.Value)
	}
	s.clearSessionCookie(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
