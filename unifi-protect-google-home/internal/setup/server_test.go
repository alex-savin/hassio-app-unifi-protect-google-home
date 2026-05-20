package setup

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSupervisor records calls and returns canned options data.
type fakeSupervisor struct {
	srv *httptest.Server

	mu          sync.Mutex
	currentOpts map[string]any
	savedOpts   map[string]any
	restarted   bool
	bearer      string
}

func newFakeSupervisor(t *testing.T) *fakeSupervisor {
	t.Helper()
	f := &fakeSupervisor{
		currentOpts: map[string]any{
			"unifi": map[string]any{
				"host": "old.example", "username": "old", "password": "old", "verify_tls": false,
			},
			"google": map[string]any{"project_id": "proj-1"},
			"bridge": map[string]any{"listen_addr": "0.0.0.0:8099"},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/addons/self/info", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.bearer = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"options": f.currentOpts},
		})
	})
	mux.HandleFunc("/addons/self/options", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Options map[string]any `json:"options"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.savedOpts = body.Options
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{"result":"ok"}`)
	})
	mux.HandleFunc("/addons/self/restart", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.restarted = true
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{"result":"ok"}`)
	})
	f.srv = httptest.NewServer(mux)
	return f
}

func (f *fakeSupervisor) close() { f.srv.Close() }

func newTestServer(sup *fakeSupervisor) *Server {
	return &Server{
		SupervisorURL: sup.srv.URL,
		Token:         "test-token",
		HTTP:          &http.Client{Timeout: 5 * time.Second},
		ScanTimeout:   100 * time.Millisecond,
	}
}

func TestIndex_ServesHTML(t *testing.T) {
	s := &Server{}
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != 200 {
		t.Fatalf("status=%d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "<title>UniFi Protect") {
		t.Fatalf("body missing title: %s", rr.Body.String()[:200])
	}
}

// staticStatus is a trivial StatusProvider for tests.
type staticStatus struct{ s StatusSnapshot }

func (x staticStatus) Status() StatusSnapshot { return x.s }

func TestStatus_NoProvider_ReturnsSetupMode(t *testing.T) {
	s := &Server{}
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rr.Code != 200 {
		t.Fatalf("status=%d", rr.Code)
	}
	var snap StatusSnapshot
	if err := json.Unmarshal(rr.Body.Bytes(), &snap); err != nil {
		t.Fatalf("json: %v", err)
	}
	if !snap.SetupMode {
		t.Fatalf("expected setup_mode=true when no provider is registered, got %+v", snap)
	}
}

func TestStatus_WithProvider_ReturnsLiveData(t *testing.T) {
	s := &Server{}
	s.SetStatus(staticStatus{s: StatusSnapshot{
		UniFi:   UniFiStatus{Host: "1.2.3.4", Connected: true, NVRMAC: "AA:BB", NVRVersion: "5.1.0"},
		Cameras: []CameraInfo{{ID: "abc", Name: "Driveway", Online: true}},
		Google:  GoogleStatus{HomeGraphEnabled: true, HomeGraphConfigured: true, ProjectID: "p"},
		Bridge:  BridgeStatus{PublicBaseURL: "https://x", PublicURLSet: true, ListenAddr: "0.0.0.0:8099"},
	}})

	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rr.Code != 200 {
		t.Fatalf("status=%d", rr.Code)
	}
	var snap StatusSnapshot
	if err := json.Unmarshal(rr.Body.Bytes(), &snap); err != nil {
		t.Fatalf("json: %v", err)
	}
	if snap.SetupMode {
		t.Fatalf("setup_mode should be false when provider returns data")
	}
	if !snap.UniFi.Connected || snap.UniFi.Host != "1.2.3.4" {
		t.Fatalf("unifi: %+v", snap.UniFi)
	}
	if len(snap.Cameras) != 1 || snap.Cameras[0].Name != "Driveway" {
		t.Fatalf("cameras: %+v", snap.Cameras)
	}
	if !snap.Google.HomeGraphConfigured || snap.Google.ProjectID != "p" {
		t.Fatalf("google: %+v", snap.Google)
	}
}

func TestSave_RejectsBadCredentials(t *testing.T) {
	// No host/username/password → probe returns error before any HTTP calls.
	sup := newFakeSupervisor(t)
	defer sup.close()
	s := newTestServer(sup)

	body, _ := json.Marshal(saveRequest{Restart: true})
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/save", bytes.NewReader(body)))

	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp saveResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.OK {
		t.Fatalf("expected ok=false, got %+v", resp)
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	if sup.savedOpts != nil {
		t.Fatalf("supervisor options were written despite invalid input")
	}
	if sup.restarted {
		t.Fatalf("add-on was restarted despite invalid input")
	}
}

func TestWriteOptions_MergesPreservingOtherGroups(t *testing.T) {
	sup := newFakeSupervisor(t)
	defer sup.close()
	s := newTestServer(sup)

	req := validateRequest{
		Host: "new.example", Username: "admin", Password: "secret", VerifyTLS: true,
	}
	if err := s.writeOptions(t.Context(), req); err != nil {
		t.Fatalf("writeOptions: %v", err)
	}

	sup.mu.Lock()
	defer sup.mu.Unlock()
	if sup.bearer != "Bearer test-token" {
		t.Fatalf("auth header: %q", sup.bearer)
	}
	if sup.savedOpts == nil {
		t.Fatalf("supervisor never received options")
	}
	u, _ := sup.savedOpts["unifi"].(map[string]any)
	if u["host"] != "new.example" || u["username"] != "admin" ||
		u["password"] != "secret" || u["verify_tls"] != true {
		t.Fatalf("unifi options not applied: %+v", u)
	}
	if g, _ := sup.savedOpts["google"].(map[string]any); g["project_id"] != "proj-1" {
		t.Fatalf("google.project_id clobbered: %+v", g)
	}
	if b, _ := sup.savedOpts["bridge"].(map[string]any); b["listen_addr"] != "0.0.0.0:8099" {
		t.Fatalf("bridge.listen_addr clobbered: %+v", b)
	}
}

func TestRestart_HitsSupervisor(t *testing.T) {
	sup := newFakeSupervisor(t)
	defer sup.close()
	s := newTestServer(sup)
	if err := s.restartAddon(t.Context()); err != nil {
		t.Fatalf("restartAddon: %v", err)
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	if !sup.restarted {
		t.Fatalf("supervisor never received restart")
	}
}

// fakeApplier records the most-recent allow-list passed to it so we can
// assert the /api/cameras handler hot-applies on success.
type fakeApplier struct {
	mu      sync.Mutex
	called  bool
	lastIDs []string
}

func (f *fakeApplier) ApplyExposedCameras(ids []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called = true
	f.lastIDs = append(f.lastIDs[:0], ids...)
}

func TestCameras_PersistsAndApplies(t *testing.T) {
	sup := newFakeSupervisor(t)
	defer sup.close()
	s := newTestServer(sup)
	app := &fakeApplier{}
	s.SetCameraApplier(app)

	body, _ := json.Marshal(camerasRequest{
		ExposedCameraIDs: []string{"cam-1", "cam-2", "cam-1", ""}, // dups + empty filtered
	})
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/cameras", bytes.NewReader(body)))

	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp camerasResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if !resp.OK {
		t.Fatalf("ok=false: %+v", resp)
	}

	// Persisted to Supervisor under bridge.exposed_cameras, normalised.
	sup.mu.Lock()
	saved, _ := sup.savedOpts["bridge"].(map[string]any)
	got, _ := saved["exposed_cameras"].([]any)
	sup.mu.Unlock()
	if len(got) != 2 || got[0] != "cam-1" || got[1] != "cam-2" {
		t.Fatalf("supervisor saw %v, want [cam-1 cam-2]", got)
	}

	// Hot-applied to the running bridge with the same normalised list.
	app.mu.Lock()
	defer app.mu.Unlock()
	if !app.called {
		t.Fatalf("applier was never called")
	}
	if len(app.lastIDs) != 2 || app.lastIDs[0] != "cam-1" || app.lastIDs[1] != "cam-2" {
		t.Fatalf("applier got %v, want [cam-1 cam-2]", app.lastIDs)
	}

	// Restart endpoint must NOT be hit — hot-apply is the whole point.
	sup.mu.Lock()
	defer sup.mu.Unlock()
	if sup.restarted {
		t.Fatalf("add-on was restarted; expected hot-apply path")
	}
}

func TestCameras_EmptyListAllowsAll(t *testing.T) {
	sup := newFakeSupervisor(t)
	defer sup.close()
	s := newTestServer(sup)
	app := &fakeApplier{}
	s.SetCameraApplier(app)

	body, _ := json.Marshal(camerasRequest{ExposedCameraIDs: []string{}})
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/cameras", bytes.NewReader(body)))
	if rr.Code != 200 {
		t.Fatalf("status=%d", rr.Code)
	}

	sup.mu.Lock()
	saved, _ := sup.savedOpts["bridge"].(map[string]any)
	got, _ := saved["exposed_cameras"].([]any)
	sup.mu.Unlock()
	if got == nil || len(got) != 0 {
		t.Fatalf("expected empty list persisted, got %v (%T)", got, got)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if !app.called || len(app.lastIDs) != 0 {
		t.Fatalf("applier got %v, want empty", app.lastIDs)
	}
}

// fakeWSLogApplier captures ApplyWSEventLog calls so we can assert the
// /api/log-settings handler invokes the hot-apply path.
type fakeWSLogApplier struct {
	mu       sync.Mutex
	called   bool
	lastLevel string
}

func (f *fakeWSLogApplier) ApplyWSEventLog(level string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called = true
	f.lastLevel = level
}

func TestLogSettings_PersistsAndApplies(t *testing.T) {
	sup := newFakeSupervisor(t)
	defer sup.close()
	s := newTestServer(sup)
	app := &fakeWSLogApplier{}
	s.SetWSLogApplier(app)

	body, _ := json.Marshal(logSettingsRequest{WSEventLog: "off"})
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/log-settings", bytes.NewReader(body)))

	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp logSettingsResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if !resp.OK || resp.WSEventLog != "off" {
		t.Fatalf("response=%+v", resp)
	}

	sup.mu.Lock()
	saved, _ := sup.savedOpts["bridge"].(map[string]any)
	got, _ := saved["ws_event_log"].(string)
	sup.mu.Unlock()
	if got != "off" {
		t.Fatalf("supervisor saw ws_event_log=%q, want off", got)
	}

	app.mu.Lock()
	defer app.mu.Unlock()
	if !app.called || app.lastLevel != "off" {
		t.Fatalf("applier got %q, want off", app.lastLevel)
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	if sup.restarted {
		t.Fatalf("add-on was restarted; expected hot-apply path")
	}
}

func TestLogSettings_NormalizesUnknownLevel(t *testing.T) {
	sup := newFakeSupervisor(t)
	defer sup.close()
	s := newTestServer(sup)
	app := &fakeWSLogApplier{}
	s.SetWSLogApplier(app)

	body, _ := json.Marshal(logSettingsRequest{WSEventLog: "bogus"})
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/log-settings", bytes.NewReader(body)))
	if rr.Code != 200 {
		t.Fatalf("status=%d", rr.Code)
	}
	sup.mu.Lock()
	saved, _ := sup.savedOpts["bridge"].(map[string]any)
	got, _ := saved["ws_event_log"].(string)
	sup.mu.Unlock()
	if got != "interesting" {
		t.Fatalf("supervisor saw ws_event_log=%q, want interesting", got)
	}
	if app.lastLevel != "interesting" {
		t.Fatalf("applier got %q, want interesting", app.lastLevel)
	}
}
