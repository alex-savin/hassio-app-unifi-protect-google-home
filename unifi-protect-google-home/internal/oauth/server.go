// Package oauth implements the minimal OAuth 2.0 authorization-code server
// required by Google account linking for Smart Home Actions.
//
// Endpoints (mounted under the public base URL):
//
//	GET  /oauth/authorize  — consent form (shared password); 302s to redirect_uri with ?code
//	POST /oauth/token      — grant_type=authorization_code | refresh_token → access_token
//
// Single-tenant: the only "user" is whoever owns the configured consent
// password. Tokens are kept in-memory; restart re-issues them via the linking
// flow.
package oauth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	codeTTL         = 10 * time.Minute
	accessTokenTTL  = 60 * time.Minute
	refreshTokenLen = 32
	accessTokenLen  = 32
)

// Server is an in-memory OAuth 2.0 authorization server.
type Server struct {
	ClientID     string
	ClientSecret string
	UserPassword string // gates the consent page; required

	mu      sync.Mutex
	codes   map[string]codeEntry
	access  map[string]accessEntry
	refresh map[string]struct{} // refresh tokens; long-lived
}

type codeEntry struct {
	RedirectURI string
	Expires     time.Time
}

type accessEntry struct {
	Expires time.Time
}

// New constructs a Server. consentPassword must be non-empty in production.
func New(clientID, clientSecret, consentPassword string) *Server {
	return &Server{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		UserPassword: consentPassword,
		codes:        map[string]codeEntry{},
		access:       map[string]accessEntry{},
		refresh:      map[string]struct{}{},
	}
}

// Authorize handles GET/POST /oauth/authorize.
//
// GET shows a consent form. POST validates the password, mints a one-time
// code, and 302s to redirect_uri?code=...&state=...
func (s *Server) Authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clientID := q.Get("client_id")
	redirectURI := q.Get("redirect_uri")
	state := q.Get("state")
	respType := q.Get("response_type")

	if clientID != s.ClientID {
		http.Error(w, "invalid client_id", http.StatusBadRequest)
		return
	}
	if respType != "code" {
		http.Error(w, "unsupported response_type", http.StatusBadRequest)
		return
	}
	if redirectURI == "" {
		http.Error(w, "missing redirect_uri", http.StatusBadRequest)
		return
	}

	if r.Method == http.MethodGet {
		consentTpl.Execute(w, consentData{
			ClientID:    clientID,
			RedirectURI: redirectURI,
			State:       state,
		})
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	pw := r.PostFormValue("password")
	if subtle.ConstantTimeCompare([]byte(pw), []byte(s.UserPassword)) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		consentTpl.Execute(w, consentData{
			ClientID:    clientID,
			RedirectURI: redirectURI,
			State:       state,
			Error:       "Incorrect password",
		})
		return
	}

	code := randomToken(24)
	s.mu.Lock()
	s.codes[code] = codeEntry{RedirectURI: redirectURI, Expires: time.Now().Add(codeTTL)}
	s.mu.Unlock()

	u, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	rq := u.Query()
	rq.Set("code", code)
	if state != "" {
		rq.Set("state", state)
	}
	u.RawQuery = rq.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// Token handles POST /oauth/token.
func (s *Server) Token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		writeTokenError(w, "invalid_request", "bad form")
		return
	}

	clientID, clientSecret, ok := basicOrFormCreds(r)
	if !ok || clientID != s.ClientID ||
		subtle.ConstantTimeCompare([]byte(clientSecret), []byte(s.ClientSecret)) != 1 {
		writeTokenError(w, "invalid_client", "bad credentials")
		return
	}

	switch r.PostFormValue("grant_type") {
	case "authorization_code":
		s.exchangeCode(w, r)
	case "refresh_token":
		s.refreshAccess(w, r)
	default:
		writeTokenError(w, "unsupported_grant_type", "")
	}
}

func (s *Server) exchangeCode(w http.ResponseWriter, r *http.Request) {
	code := r.PostFormValue("code")
	redirectURI := r.PostFormValue("redirect_uri")

	s.mu.Lock()
	entry, ok := s.codes[code]
	if ok {
		delete(s.codes, code) // one-time
	}
	s.mu.Unlock()
	if !ok || time.Now().After(entry.Expires) || entry.RedirectURI != redirectURI {
		writeTokenError(w, "invalid_grant", "code invalid or expired")
		return
	}

	access := randomToken(accessTokenLen)
	refresh := randomToken(refreshTokenLen)
	s.mu.Lock()
	s.access[access] = accessEntry{Expires: time.Now().Add(accessTokenTTL)}
	s.refresh[refresh] = struct{}{}
	s.mu.Unlock()

	writeTokenJSON(w, access, refresh)
}

func (s *Server) refreshAccess(w http.ResponseWriter, r *http.Request) {
	rt := r.PostFormValue("refresh_token")
	s.mu.Lock()
	_, ok := s.refresh[rt]
	s.mu.Unlock()
	if !ok {
		writeTokenError(w, "invalid_grant", "unknown refresh_token")
		return
	}
	access := randomToken(accessTokenLen)
	s.mu.Lock()
	s.access[access] = accessEntry{Expires: time.Now().Add(accessTokenTTL)}
	s.mu.Unlock()
	writeTokenJSON(w, access, "")
}

// Validate returns nil if accessToken is currently valid.
func (s *Server) Validate(accessToken string) error {
	s.mu.Lock()
	entry, ok := s.access[accessToken]
	s.mu.Unlock()
	if !ok {
		return ErrInvalidToken
	}
	if time.Now().After(entry.Expires) {
		return ErrInvalidToken
	}
	return nil
}

// ErrInvalidToken is returned by Validate for unknown/expired tokens.
var ErrInvalidToken = errors.New("oauth: invalid or expired token")

// --- helpers ---

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func basicOrFormCreds(r *http.Request) (id, secret string, ok bool) {
	if u, p, hasBasic := r.BasicAuth(); hasBasic {
		return u, p, true
	}
	id = r.PostFormValue("client_id")
	secret = r.PostFormValue("client_secret")
	if id == "" {
		return "", "", false
	}
	return id, secret, true
}

func writeTokenJSON(w http.ResponseWriter, access, refresh string) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{
		"access_token": access,
		"token_type":   "Bearer",
		"expires_in":   int(accessTokenTTL.Seconds()),
	}
	if refresh != "" {
		resp["refresh_token"] = refresh
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func writeTokenError(w http.ResponseWriter, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	body := map[string]string{"error": code}
	if desc != "" {
		body["error_description"] = desc
	}
	_ = json.NewEncoder(w).Encode(body)
}

func init() {
	consentTpl = template.Must(template.New("consent").Parse(consentHTML))
}

var consentTpl *template.Template

type consentData struct {
	ClientID    string
	RedirectURI string
	State       string
	Error       string
}

const consentHTML = `<!doctype html>
<html><head><meta charset="utf-8"><title>Authorize</title>
<style>body{font-family:sans-serif;max-width:420px;margin:4em auto;padding:1em}
input{font-size:1em;padding:.5em;width:100%;box-sizing:border-box;margin:.5em 0}
button{font-size:1em;padding:.6em 1.2em;background:#1a73e8;color:#fff;border:0;border-radius:4px;cursor:pointer}
.err{color:#c00;margin:.5em 0}</style></head>
<body>
<h2>Link Google Home to your cameras</h2>
<p>Enter the consent password to authorize Google to access your UniFi Protect cameras.</p>
{{if .Error}}<div class="err">{{.Error}}</div>{{end}}
<form method="POST">
<input type="password" name="password" placeholder="Consent password" autofocus required>
<input type="hidden" name="client_id" value="{{.ClientID}}">
<input type="hidden" name="redirect_uri" value="{{.RedirectURI}}">
<input type="hidden" name="state" value="{{.State}}">
<button type="submit">Authorize</button>
</form>
</body></html>`
