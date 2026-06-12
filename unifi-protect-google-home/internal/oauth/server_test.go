package oauth

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	testClientID     = "client-id"
	testClientSecret = "client-secret"
	testPassword     = "test-password-123"
	testRedirectURI  = "https://oauth-redirect.googleusercontent.com/r/test-project"
	testState        = "st-123"
)

func newTestServer() *Server {
	return New(testClientID, testClientSecret, testPassword, []byte("0123456789abcdef0123456789abcdef"))
}

// authorizeQuery builds the canonical query string for /oauth/authorize.
func authorizeQuery(clientID, redirectURI, respType string) string {
	q := url.Values{
		"client_id":     {clientID},
		"response_type": {respType},
		"state":         {testState},
	}
	if redirectURI != "" {
		q.Set("redirect_uri", redirectURI)
	}
	return q.Encode()
}

// newAuthorizePOST builds a consent-form submission. The form posts back to
// the same URL, so the OAuth parameters ride on the query string and only the
// password travels in the body.
func newAuthorizePOST(password string) *http.Request {
	req := httptest.NewRequest(http.MethodPost,
		"/oauth/authorize?"+authorizeQuery(testClientID, testRedirectURI, "code"),
		strings.NewReader(url.Values{"password": {password}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func doAuthorize(s *Server, req *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	s.Authorize(rr, req)
	return rr
}

// obtainCode runs a successful consent POST and extracts the one-time code
// from the redirect Location.
func obtainCode(t *testing.T, s *Server) string {
	t.Helper()
	rr := doAuthorize(s, newAuthorizePOST(testPassword))
	if rr.Code != http.StatusFound {
		t.Fatalf("authorize status=%d body=%s", rr.Code, rr.Body.String())
	}
	loc, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("no code in redirect: %s", rr.Header().Get("Location"))
	}
	return code
}

func postToken(s *Server, form url.Values, basicAuth bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basicAuth {
		req.SetBasicAuth(testClientID, testClientSecret)
	}
	rr := httptest.NewRecorder()
	s.Token(rr, req)
	return rr
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
}

func decodeToken(t *testing.T, rr *httptest.ResponseRecorder) tokenResponse {
	t.Helper()
	var resp tokenResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode token response: %v body=%s", err, rr.Body.String())
	}
	return resp
}

// --- /oauth/authorize ---

func TestAuthorizeGET_RendersConsentForm(t *testing.T) {
	s := newTestServer()
	req := httptest.NewRequest(http.MethodGet,
		"/oauth/authorize?"+authorizeQuery(testClientID, testRedirectURI, "code"), nil)
	rr := doAuthorize(s, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{`name="password"`, testRedirectURI, testState} {
		if !strings.Contains(body, want) {
			t.Errorf("consent form missing %q", want)
		}
	}
}

func TestAuthorizeGET_Rejections(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{"wrong client_id", authorizeQuery("other-client", testRedirectURI, "code")},
		{"unsupported response_type", authorizeQuery(testClientID, testRedirectURI, "token")},
		{"missing redirect_uri", authorizeQuery(testClientID, "", "code")},
		{"non-google redirect_uri", authorizeQuery(testClientID, "https://evil.example/cb", "code")},
		{"non-https google redirect_uri", authorizeQuery(testClientID, "http://oauth-redirect.googleusercontent.com/r/test-project", "code")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer()
			rr := doAuthorize(s, httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+tc.query, nil))
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400", rr.Code)
			}
		})
	}
}

func TestAuthorizeGET_AcceptsSandboxRedirect(t *testing.T) {
	s := newTestServer()
	q := authorizeQuery(testClientID, "https://oauth-redirect-sandbox.googleusercontent.com/r/test-project", "code")
	rr := doAuthorize(s, httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rr.Code)
	}
}

func TestAuthorizePOST_WrongPassword(t *testing.T) {
	s := newTestServer()
	rr := doAuthorize(s, newAuthorizePOST("wrong-password"))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Incorrect password") {
		t.Fatalf("body missing error message: %s", rr.Body.String())
	}
}

func TestAuthorizePOST_CorrectPassword_RedirectsWithCodeAndState(t *testing.T) {
	s := newTestServer()
	rr := doAuthorize(s, newAuthorizePOST(testPassword))
	if rr.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302", rr.Code)
	}
	loc, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if got := loc.Scheme + "://" + loc.Host + loc.Path; got != testRedirectURI {
		t.Errorf("redirect target=%q, want %q", got, testRedirectURI)
	}
	if loc.Query().Get("code") == "" {
		t.Errorf("no code in redirect: %s", loc)
	}
	if got := loc.Query().Get("state"); got != testState {
		t.Errorf("state=%q, want %q", got, testState)
	}
}

func TestAuthorizePOST_PerIPRateLimit(t *testing.T) {
	s := newTestServer()
	const ip = "203.0.113.7:51000"
	for i := 0; i < pwMaxPerIP; i++ {
		req := newAuthorizePOST("wrong-password")
		req.RemoteAddr = ip
		if rr := doAuthorize(s, req); rr.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status=%d, want 401", i+1, rr.Code)
		}
	}
	// Budget exhausted: even the correct password is refused from this IP.
	req := newAuthorizePOST(testPassword)
	req.RemoteAddr = ip
	if rr := doAuthorize(s, req); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429 after %d failures", rr.Code, pwMaxPerIP)
	}
	// A different IP is still allowed through.
	req = newAuthorizePOST(testPassword)
	req.RemoteAddr = "198.51.100.9:51000"
	if rr := doAuthorize(s, req); rr.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302 from an unthrottled IP", rr.Code)
	}
}

func TestAuthorizePOST_GlobalRateLimit(t *testing.T) {
	s := newTestServer()
	// Rotating X-Forwarded-For dodges the per-IP limit but not the global cap.
	for i := 0; i < pwMaxGlobal; i++ {
		req := newAuthorizePOST("wrong-password")
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("10.0.%d.%d", i/250, i%250+1))
		if rr := doAuthorize(s, req); rr.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status=%d, want 401", i+1, rr.Code)
		}
	}
	req := newAuthorizePOST(testPassword)
	req.Header.Set("X-Forwarded-For", "10.99.99.99")
	if rr := doAuthorize(s, req); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429 after %d global failures", rr.Code, pwMaxGlobal)
	}
}

// --- /oauth/token ---

func TestToken_AuthorizationCodeFlow_FormCreds(t *testing.T) {
	s := newTestServer()
	code := obtainCode(t, s)
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {testRedirectURI},
		"client_id":     {testClientID},
		"client_secret": {testClientSecret},
	}
	rr := postToken(s, form, false)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	resp := decodeToken(t, rr)
	if resp.AccessToken == "" || resp.RefreshToken == "" {
		t.Fatalf("missing tokens: %+v", resp)
	}
	if resp.TokenType != "Bearer" {
		t.Errorf("token_type=%q, want Bearer", resp.TokenType)
	}
	if err := s.Validate(resp.AccessToken); err != nil {
		t.Errorf("Validate(access)=%v, want nil", err)
	}

	// The code is one-time: replaying the exchange must fail.
	rr = postToken(s, form, false)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("code reuse status=%d, want 400", rr.Code)
	}
	if got := decodeToken(t, rr).Error; got != "invalid_grant" {
		t.Errorf("code reuse error=%q, want invalid_grant", got)
	}
}

func TestToken_AuthorizationCodeFlow_BasicAuth(t *testing.T) {
	s := newTestServer()
	form := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {obtainCode(t, s)},
		"redirect_uri": {testRedirectURI},
	}
	rr := postToken(s, form, true)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	resp := decodeToken(t, rr)
	if resp.AccessToken == "" || resp.RefreshToken == "" {
		t.Fatalf("missing tokens: %+v", resp)
	}
	if err := s.Validate(resp.AccessToken); err != nil {
		t.Errorf("Validate(access)=%v, want nil", err)
	}
}

func TestToken_MismatchedRedirectURI(t *testing.T) {
	s := newTestServer()
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {obtainCode(t, s)},
		"redirect_uri":  {"https://oauth-redirect.googleusercontent.com/r/other-project"},
		"client_id":     {testClientID},
		"client_secret": {testClientSecret},
	}
	rr := postToken(s, form, false)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rr.Code)
	}
	if got := decodeToken(t, rr).Error; got != "invalid_grant" {
		t.Errorf("error=%q, want invalid_grant", got)
	}
}

func TestToken_WrongClientSecret(t *testing.T) {
	s := newTestServer()
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {obtainCode(t, s)},
		"redirect_uri":  {testRedirectURI},
		"client_id":     {testClientID},
		"client_secret": {"not-the-secret"},
	}
	rr := postToken(s, form, false)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rr.Code)
	}
	if got := decodeToken(t, rr).Error; got != "invalid_client" {
		t.Errorf("error=%q, want invalid_client", got)
	}
}

func TestToken_RefreshGrant(t *testing.T) {
	s := newTestServer()
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {obtainCode(t, s)},
		"redirect_uri":  {testRedirectURI},
		"client_id":     {testClientID},
		"client_secret": {testClientSecret},
	}
	first := decodeToken(t, postToken(s, form, false))
	if first.RefreshToken == "" {
		t.Fatalf("no refresh token issued: %+v", first)
	}

	rr := postToken(s, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {first.RefreshToken},
		"client_id":     {testClientID},
		"client_secret": {testClientSecret},
	}, false)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	refreshed := decodeToken(t, rr)
	if refreshed.AccessToken == "" {
		t.Fatalf("no access token from refresh grant: %+v", refreshed)
	}
	if err := s.Validate(refreshed.AccessToken); err != nil {
		t.Errorf("Validate(refreshed access)=%v, want nil", err)
	}
}

func TestToken_GarbageRefreshToken(t *testing.T) {
	s := newTestServer()
	rr := postToken(s, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {"not.a.real.token"},
		"client_id":     {testClientID},
		"client_secret": {testClientSecret},
	}, false)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rr.Code)
	}
	if got := decodeToken(t, rr).Error; got != "invalid_grant" {
		t.Errorf("error=%q, want invalid_grant", got)
	}
}

// --- Validate ---

func TestValidate(t *testing.T) {
	s := newTestServer()

	access, err := s.mintToken(tokenKindAccess, time.Minute)
	if err != nil {
		t.Fatalf("mintToken(access): %v", err)
	}
	if err := s.Validate(access); err != nil {
		t.Errorf("Validate(access)=%v, want nil", err)
	}

	refresh, err := s.mintToken(tokenKindRefresh, time.Minute)
	if err != nil {
		t.Fatalf("mintToken(refresh): %v", err)
	}
	if err := s.Validate(refresh); err == nil {
		t.Error("Validate accepted a refresh token as an access token")
	}

	// Tampered signature: flip the last base64url character.
	last := access[len(access)-1]
	flip := byte('A')
	if last == 'A' {
		flip = 'B'
	}
	if err := s.Validate(access[:len(access)-1] + string(flip)); err == nil {
		t.Error("Validate accepted a token with a tampered signature")
	}

	// A server with a different signing secret must reject the token.
	other := New(testClientID, testClientSecret, testPassword, []byte("ffffffffffffffffffffffffffffffff"))
	if err := other.Validate(access); err == nil {
		t.Error("Validate accepted a token signed with a different secret")
	}
}
