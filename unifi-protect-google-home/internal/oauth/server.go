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
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	codeTTL = 10 * time.Minute
	// accessTokenTTL is short-lived; Google refreshes via the refresh token.
	accessTokenTTL = 60 * time.Minute
	// refreshTokenTTL is effectively "never expires" — Google's smart home
	// account linking expects refresh tokens that don't lapse, otherwise
	// the user gets silently unlinked.
	refreshTokenTTL = 10 * 365 * 24 * time.Hour

	// Consent-password brute-force limits. The endpoint is internet-exposed
	// (Google must reach it), so failed attempts are rate limited per IP
	// with a global backstop (X-Forwarded-For is client-controlled, so a
	// per-IP limit alone could be rotated around).
	pwFailWindow = 15 * time.Minute
	pwMaxPerIP   = 5
	pwMaxGlobal  = 20
)

// token kinds
const (
	tokenKindAccess  = "a"
	tokenKindRefresh = "r"
)

// Server is an OAuth 2.0 authorization server.
//
// Authorization codes are kept in memory (one-time, 10-minute TTL — used
// immediately during the link flow, so process restarts mid-link are not a
// real-world concern). Access and refresh tokens are *stateless*: HMAC-signed
// strings with an embedded expiry, so they survive add-on restarts without
// needing on-disk storage. This prevents Google from de-binding the user
// when our short in-memory cache is wiped by a restart.
type Server struct {
	ClientID     string
	ClientSecret string
	UserPassword string // gates the consent page; required
	TokenSecret  []byte // HMAC-SHA256 key for signing access/refresh tokens

	mu      sync.Mutex
	codes   map[string]codeEntry
	pwFails map[string][]time.Time // failed consent attempts per client IP
}

type codeEntry struct {
	RedirectURI string
	Expires     time.Time
}

// New constructs a Server. consentPassword and tokenSecret must be non-empty
// in production.
func New(clientID, clientSecret, consentPassword string, tokenSecret []byte) *Server {
	return &Server{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		UserPassword: consentPassword,
		TokenSecret:  tokenSecret,
		codes:        map[string]codeEntry{},
		pwFails:      map[string][]time.Time{},
	}
}

// validRedirectURI accepts only Google's account-linking redirector
// (https://oauth-redirect[-sandbox].googleusercontent.com/r/<project-id>).
// Accepting arbitrary URIs would make this internet-exposed endpoint an
// open redirect and let an authorization code be delivered to an
// attacker-controlled destination.
func validRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return false
	}
	switch u.Hostname() {
	case "oauth-redirect.googleusercontent.com",
		"oauth-redirect-sandbox.googleusercontent.com":
		return true
	}
	return false
}

// clientIP extracts the requester's IP, trusting the first X-Forwarded-For
// hop when present (the bridge sits behind the user's reverse proxy).
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			xff = xff[:i]
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// tooManyFailures prunes expired entries and reports whether ip (or the
// instance globally) has exceeded the failed-consent budget.
func (s *Server) tooManyFailures(ip string) bool {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for k, ts := range s.pwFails {
		kept := ts[:0]
		for _, t := range ts {
			if now.Sub(t) < pwFailWindow {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(s.pwFails, k)
			continue
		}
		s.pwFails[k] = kept
		total += len(kept)
	}
	return len(s.pwFails[ip]) >= pwMaxPerIP || total >= pwMaxGlobal
}

func (s *Server) recordFailure(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pwFails == nil {
		s.pwFails = map[string][]time.Time{}
	}
	s.pwFails[ip] = append(s.pwFails[ip], time.Now())
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
	if !validRedirectURI(redirectURI) {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}

	if r.Method == http.MethodGet {
		_ = consentTpl.Execute(w, consentData{
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
	ip := clientIP(r)
	if s.tooManyFailures(ip) {
		http.Error(w, "too many failed attempts — try again later", http.StatusTooManyRequests)
		return
	}
	pw := r.PostFormValue("password")
	if subtle.ConstantTimeCompare([]byte(pw), []byte(s.UserPassword)) != 1 {
		s.recordFailure(ip)
		w.WriteHeader(http.StatusUnauthorized)
		_ = consentTpl.Execute(w, consentData{
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
	// Not an open redirect: redirectURI was validated against Google's
	// fixed account-linking hosts by validRedirectURI above.
	http.Redirect(w, r, u.String(), http.StatusFound) //nolint:gosec
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

	access, err := s.mintToken(tokenKindAccess, accessTokenTTL)
	if err != nil {
		writeTokenError(w, "server_error", "mint access")
		return
	}
	refresh, err := s.mintToken(tokenKindRefresh, refreshTokenTTL)
	if err != nil {
		writeTokenError(w, "server_error", "mint refresh")
		return
	}
	writeTokenJSON(w, access, refresh)
}

func (s *Server) refreshAccess(w http.ResponseWriter, r *http.Request) {
	rt := r.PostFormValue("refresh_token")
	if err := s.verifyToken(rt, tokenKindRefresh); err != nil {
		writeTokenError(w, "invalid_grant", "refresh_token "+err.Error())
		return
	}
	access, err := s.mintToken(tokenKindAccess, accessTokenTTL)
	if err != nil {
		writeTokenError(w, "server_error", "mint access")
		return
	}
	writeTokenJSON(w, access, "")
}

// Validate returns nil if accessToken is currently valid.
func (s *Server) Validate(accessToken string) error {
	return s.verifyToken(accessToken, tokenKindAccess)
}

// mintToken builds an HMAC-signed, self-describing token of the form
//
//	<kind>.<exp_unix>.<nonce_b64url>.<hmac_b64url>
//
// The HMAC covers "<kind>.<exp>.<nonce>". Tokens are stateless, so they
// survive add-on restarts without on-disk storage.
func (s *Server) mintToken(kind string, ttl time.Duration) (string, error) {
	if len(s.TokenSecret) == 0 {
		return "", errors.New("oauth: token secret not configured")
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	exp := time.Now().Add(ttl).Unix()
	payload := fmt.Sprintf("%s.%d.%s", kind, exp, base64.RawURLEncoding.EncodeToString(nonce))
	mac := hmac.New(sha256.New, s.TokenSecret)
	_, _ = mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return payload + "." + sig, nil
}

// verifyToken parses, checks expiry, and verifies the HMAC of an issued token.
func (s *Server) verifyToken(tok, wantKind string) error {
	if len(s.TokenSecret) == 0 {
		return ErrInvalidToken
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 4 {
		return ErrInvalidToken
	}
	kind, expStr, nonce, sig := parts[0], parts[1], parts[2], parts[3]
	if kind != wantKind {
		return ErrInvalidToken
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return ErrInvalidToken
	}
	if time.Now().Unix() > exp {
		return ErrInvalidToken
	}
	payload := kind + "." + expStr + "." + nonce
	mac := hmac.New(sha256.New, s.TokenSecret)
	_, _ = mac.Write([]byte(payload))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(want), []byte(sig)) != 1 {
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
