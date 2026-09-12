// Package auth: autenticación del panel. Sesión HMAC firmada en cookie HttpOnly.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// CookieName es el nombre de la cookie de sesión.
const CookieName = "s1_session"

// Session es el payload firmado dentro de la cookie.
type Session struct {
	User   string    `json:"user"`
	Expiry time.Time `json:"expiry"`
}

// Manager firma y valida sesiones HMAC-SHA256.
type Manager struct {
	secret []byte
	ttl    time.Duration
}

// NewManager crea un Manager con el secreto y TTL dados.
func NewManager(secret string, ttl time.Duration) (*Manager, error) {
	if secret == "" {
		return nil, fmt.Errorf("SESSION_SECRET vacío")
	}
	if len(secret) < 16 {
		return nil, fmt.Errorf("SESSION_SECRET debe tener al menos 16 caracteres")
	}
	return &Manager{secret: []byte(secret), ttl: ttl}, nil
}

// Sign construye el token: base64(payload) . base64(hmac_sha256(payload)).
func (m *Manager) Sign(s Session) (string, error) {
	payload, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, m.secret)
	mac.Write(payload)
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(sig), nil
}

// Verify valida firma y expiración. Devuelve la sesión si es válida.
func (m *Manager) Verify(token string) (*Session, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return nil, fmt.Errorf("formato de token inválido")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("payload inválido")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("firma inválida")
	}
	mac := hmac.New(sha256.New, m.secret)
	mac.Write(payload)
	if !hmac.Equal(mac.Sum(nil), sig) {
		return nil, fmt.Errorf("firma no coincide")
	}
	var s Session
	if err := json.Unmarshal(payload, &s); err != nil {
		return nil, fmt.Errorf("sesión corrupta")
	}
	if time.Now().After(s.Expiry) {
		return nil, fmt.Errorf("sesión expirada")
	}
	return &s, nil
}

// SetCookie escribe la cookie de sesión (HttpOnly, SameSite=Lax).
func (m *Manager) SetCookie(w http.ResponseWriter, user string, secure bool) error {
	tok, err := m.Sign(Session{User: user, Expiry: time.Now().Add(m.ttl)})
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(m.ttl.Seconds()),
	})
	return nil
}

// ClearCookie elimina la cookie de sesión.
func ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
}

// UserFromCookie devuelve el usuario si la cookie es válida, o "".
func (m *Manager) UserFromCookie(r *http.Request) string {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return ""
	}
	s, err := m.Verify(c.Value)
	if err != nil {
		return ""
	}
	return s.User
}

// RandToken genera un token aleatorio (para futuros tokens TUI y control).
func RandToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
