package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/ghome"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/oauth"
)

const (
	testClientID     = "test-client"
	testClientSecret = "test-client-secret"
	testConsentPW    = "test-consent-pw"
	testRedirectURI  = "https://oauth-redirect.googleusercontent.com/r/test"
)

// fakeSource is a minimal ghome.Source so the fulfillment handler can be
// exercised through the public mux without any UniFi plumbing.
type fakeSource struct{}

func (fakeSource) ListCameras() []ghome.Camera {
	return []ghome.Camera{{ID: "cam1", Name: "Driveway", Manufacturer: "Ubiquiti", Model: "G4 Bullet", Online: true}}
}

func (fakeSource) SignalingURL(camID string) (string, error) {
	return "https://bridge.example/webrtc/signal?cam=" + camID, nil
}

func (fakeSource) HLSURL(camID string) (string, error) {
	return "https://bridge.example/hls/" + camID + "/index.m3u8", nil
}

func (fakeSource) ProgressiveMP4URL(camID string) (string, string, error) {
	return "https://bridge.example/mp4/" + camID + "/stream.mp4", "fake-token", nil
}

func newTestServer() *Server {
	return &Server{
		PublicBaseURL:     "https://bridge.example",
		StreamTokenSecret: []byte("test-secret-32-bytes-aaaaaaaaaaa"),
		OAuth:             oauth.New(testClientID, testClientSecret, testConsentPW, []byte("oauth-token-secret")),
		Fulfill:           &ghome.Handler{Source: fakeSource{}},
	}
}

// --- signToken / verifyToken ---

func TestVerifyToken_RoundTrip(t *testing.T) {
	s := newTestServer()
	exp := time.Now().Add(time.Minute).Unix()
	tok := s.signToken("cam1", exp)
	if err := s.verifyToken("cam1", strconv.FormatInt(exp, 10), tok); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
}

func TestVerifyToken_Expired(t *testing.T) {
	s := newTestServer()
	exp := time.Now().Add(-time.Minute).Unix()
	tok := s.signToken("cam1", exp)
	if err := s.verifyToken("cam1", strconv.FormatInt(exp, 10), tok); err == nil {
		t.Fatal("expired token accepted")
	}
}

func TestVerifyToken_WrongCamID(t *testing.T) {
	s := newTestServer()
	exp := time.Now().Add(time.Minute).Unix()
	tok := s.signToken("cam1", exp)
	if err := s.verifyToken("cam2", strconv.FormatInt(exp, 10), tok); err == nil {
		t.Fatal("token signed for cam1 accepted for cam2")
	}
}

func TestVerifyToken_TamperedSig(t *testing.T) {
	s := newTestServer()
	exp := time.Now().Add(time.Minute).Unix()
	tok := s.signToken("cam1", exp)
	flipped := "A"
	if tok[0] == 'A' {
		flipped = "B"
	}
	tampered := flipped + tok[1:]
	if err := s.verifyToken("cam1", strconv.FormatInt(exp, 10), tampered); err == nil {
		t.Fatal("tampered signature accepted")
	}
}

func TestVerifyToken_NonNumericExp(t *testing.T) {
	s := newTestServer()
	tok := s.signToken("cam1", time.Now().Add(time.Minute).Unix())
	if err := s.verifyToken("cam1", "not-a-number", tok); err == nil {
		t.Fatal("non-numeric exp accepted")
	}
}

// --- URL builders ---

func TestSignalingURL_Shape(t *testing.T) {
	s := newTestServer()
	raw, err := s.SignalingURL("cam1")
	if err != nil {
		t.Fatalf("SignalingURL: %v", err)
	}
	if !strings.HasPrefix(raw, "https://bridge.example/webrtc/signal?") {
		t.Fatalf("unexpected prefix: %s", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	q := u.Query()
	if q.Get("cam") != "cam1" {
		t.Fatalf("cam=%q, want cam1", q.Get("cam"))
	}
	exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	if err != nil {
		t.Fatalf("exp not numeric: %q", q.Get("exp"))
	}
	if exp <= time.Now().Unix() {
		t.Fatalf("exp %d not in the future", exp)
	}
	if err := s.verifyToken("cam1", q.Get("exp"), q.Get("t")); err != nil {
		t.Fatalf("embedded token does not verify: %v", err)
	}
}

func TestHLSURL_Shape(t *testing.T) {
	s := newTestServer()
	raw, err := s.HLSURL("cam1")
	if err != nil {
		t.Fatalf("HLSURL: %v", err)
	}
	if !strings.HasPrefix(raw, "https://bridge.example/hls/cam1/") {
		t.Fatalf("unexpected prefix: %s", raw)
	}
	if !strings.HasSuffix(raw, "/index.m3u8") {
		t.Fatalf("unexpected suffix: %s", raw)
	}
	// Path layout: /hls/<cam>/<exp>/<sig>/index.m3u8
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/hls/"), "/")
	if len(parts) != 4 {
		t.Fatalf("path has %d segments after /hls/, want 4: %s", len(parts), u.Path)
	}
	camID, expStr, sig, rest := parts[0], parts[1], parts[2], parts[3]
	if camID != "cam1" || rest != "index.m3u8" {
		t.Fatalf("unexpected path segments: %v", parts)
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		t.Fatalf("exp not numeric: %q", expStr)
	}
	if exp <= time.Now().Unix() {
		t.Fatalf("exp %d not in the future", exp)
	}
	if err := s.verifyToken(camID, expStr, sig); err != nil {
		t.Fatalf("embedded token does not verify: %v", err)
	}
}

func TestProgressiveMP4URL_Shape(t *testing.T) {
	s := newTestServer()
	raw, tok, err := s.ProgressiveMP4URL("cam1")
	if err != nil {
		t.Fatalf("ProgressiveMP4URL: %v", err)
	}
	if raw != "https://bridge.example/mp4/cam1/stream.mp4" {
		t.Fatalf("unexpected url: %s", raw)
	}
	// Token format: "<exp>.<sig>".
	dot := strings.IndexByte(tok, '.')
	if dot <= 0 {
		t.Fatalf("token missing dot separator: %q", tok)
	}
	expStr, sig := tok[:dot], tok[dot+1:]
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		t.Fatalf("exp not numeric: %q", expStr)
	}
	if exp <= time.Now().Unix() {
		t.Fatalf("exp %d not in the future", exp)
	}
	if sig != s.signToken("cam1", exp) {
		t.Fatalf("sig does not match signToken output: %q", sig)
	}
	if err := s.verifyToken("cam1", expStr, sig); err != nil {
		t.Fatalf("token does not verify: %v", err)
	}
}

// --- authMiddleware on /smarthome ---

// mintAccessToken drives the full authorize+token OAuth exchange against the
// public mux and returns a real access token.
func mintAccessToken(t *testing.T, h http.Handler) string {
	t.Helper()

	q := url.Values{
		"client_id":     {testClientID},
		"redirect_uri":  {testRedirectURI},
		"response_type": {"code"},
		"state":         {"st-1"},
	}
	form := url.Values{"password": {testConsentPW}}
	req := httptest.NewRequest(http.MethodPost, "/oauth/authorize?"+q.Encode(), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("authorize status=%d body=%s", rr.Code, rr.Body.String())
	}
	loc, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect: %v", err)
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("redirect missing code: %s", loc)
	}
	if loc.Query().Get("state") != "st-1" {
		t.Fatalf("redirect missing state: %s", loc)
	}

	form = url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {testRedirectURI},
		"client_id":     {testClientID},
		"client_secret": {testClientSecret},
	}
	req = httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("token status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("token json: %v", err)
	}
	if resp.AccessToken == "" {
		t.Fatalf("empty access_token: %s", rr.Body.String())
	}
	return resp.AccessToken
}

func TestSmartHome_MissingAuthorization(t *testing.T) {
	h := newTestServer().Routes()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/smarthome", strings.NewReader("{}")))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rr.Code)
	}
}

func TestSmartHome_GarbageBearer(t *testing.T) {
	h := newTestServer().Routes()
	req := httptest.NewRequest(http.MethodPost, "/smarthome", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer garbage")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rr.Code)
	}
}

func TestSmartHome_ValidBearer_ReachesFulfillment(t *testing.T) {
	h := newTestServer().Routes()
	tok := mintAccessToken(t, h)

	body := `{"requestId":"req-1","inputs":[{"intent":"action.devices.SYNC"}]}`
	req := httptest.NewRequest(http.MethodPost, "/smarthome", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	var resp struct {
		RequestID string `json:"requestId"`
		Payload   struct {
			Devices []struct {
				ID string `json:"id"`
			} `json:"devices"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("sync json: %v body=%s", err, rr.Body.String())
	}
	if resp.RequestID != "req-1" {
		t.Fatalf("requestId=%q, want req-1", resp.RequestID)
	}
	if len(resp.Payload.Devices) != 1 || resp.Payload.Devices[0].ID != "cam1" {
		t.Fatalf("unexpected SYNC devices: %s", rr.Body.String())
	}
}

// --- hlsHandler ---

func TestHLSHandler_BadPath(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	s.hlsHandler(rr, httptest.NewRequest(http.MethodGet, "/hls/cam1/12345", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rr.Code)
	}
}

func TestHLSHandler_InvalidToken(t *testing.T) {
	s := newTestServer()
	exp := time.Now().Add(time.Hour).Unix()
	path := "/hls/cam1/" + strconv.FormatInt(exp, 10) + "/bad-signature/index.m3u8"
	rr := httptest.NewRecorder()
	s.hlsHandler(rr, httptest.NewRequest(http.MethodGet, path, nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rr.Code)
	}
}

// --- mp4Handler ---

func TestMP4Handler_MissingBearer(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	s.mp4Handler(rr, httptest.NewRequest(http.MethodGet, "/mp4/cam1/stream.mp4", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rr.Code)
	}
}

func TestMP4Handler_MalformedToken(t *testing.T) {
	s := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/mp4/cam1/stream.mp4", nil)
	req.Header.Set("Authorization", "Bearer no-dot-in-token")
	rr := httptest.NewRecorder()
	s.mp4Handler(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rr.Code)
	}
}

func TestMP4Handler_MethodNotAllowed(t *testing.T) {
	s := newTestServer()
	exp := time.Now().Add(time.Hour).Unix()
	tok := strconv.FormatInt(exp, 10) + "." + s.signToken("cam1", exp)
	req := httptest.NewRequest(http.MethodPost, "/mp4/cam1/stream.mp4", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	s.mp4Handler(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d, want 405", rr.Code)
	}
}

// --- Routes ---

func TestRoutes_Healthz(t *testing.T) {
	h := newTestServer().Routes()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rr.Code)
	}
	if rr.Body.String() != "ok" {
		t.Fatalf("body=%q, want ok", rr.Body.String())
	}
}

func TestRoutes_NoAdminDiscover(t *testing.T) {
	h := newTestServer().Routes()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/discover", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (admin endpoints must not be public)", rr.Code)
	}
}
