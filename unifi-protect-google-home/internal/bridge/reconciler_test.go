package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/config"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/streams"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/unifi"
)

// fakeController serves a mutable one-camera bootstrap. gate, when set,
// blocks each bootstrap request until a value is received from it.
type fakeController struct {
	mu       sync.Mutex
	state    string
	alias    string
	rtspOn   bool
	present  bool
	gate     chan struct{}
	inFlight atomic.Int32
	maxSeen  atomic.Int32
	srv      *httptest.Server
}

func newFakeController(t *testing.T) *fakeController {
	t.Helper()
	f := &fakeController{state: "CONNECTED", alias: "alias-a", rtspOn: true, present: true}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "TOKEN", Value: "s", Path: "/"})
	})
	mux.HandleFunc("/proxy/protect/api/bootstrap", func(w http.ResponseWriter, _ *http.Request) {
		n := f.inFlight.Add(1)
		defer f.inFlight.Add(-1)
		for {
			m := f.maxSeen.Load()
			if n <= m || f.maxSeen.CompareAndSwap(m, n) {
				break
			}
		}
		f.mu.Lock()
		body := f.bootstrapLocked()
		gate := f.gate
		f.mu.Unlock()
		if gate != nil {
			<-gate
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeController) bootstrapLocked() map[string]any {
	cams := []any{}
	if f.present {
		cams = append(cams, map[string]any{
			"id": "cam-1", "name": "Front", "modelKey": "camera", "type": "G4 Bullet",
			"state": f.state,
			"channels": []any{map[string]any{
				"id": 0, "width": 1920, "height": 1080,
				"rtspAlias": f.alias, "isRtspEnabled": f.rtspOn,
			}},
		})
	}
	return map[string]any{"lastUpdateId": "u-1", "cameras": cams}
}

func (f *fakeController) update(fn func(f *fakeController)) {
	f.mu.Lock()
	fn(f)
	f.mu.Unlock()
}

func newTestReconciler(t *testing.T, f *fakeController) (*Reconciler, *streams.Registry, *CameraSource) {
	t.Helper()
	uc := unifi.New(config.UniFi{Host: strings.TrimPrefix(f.srv.URL, "https://"), Username: "u", Password: "p"})
	reg := streams.NewRegistry()
	src := NewCameraSource(nil)
	return NewReconciler(uc, reg, src, nil, "agent", false, "off"), reg, src
}

// TestRefreshTracksRTSPURL locks finding #2: a changed RTSP alias must
// re-register the stream, disabling RTSP or removing the camera must
// unregister it, and each change must notify the stream servers.
func TestRefreshTracksRTSPURL(t *testing.T) {
	f := newFakeController(t)
	r, reg, src := newTestReconciler(t, f)
	var notified atomic.Int32
	src.OnStreamsChanged(func() { notified.Add(1) })
	ctx := context.Background()

	if _, err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	first, ok := reg.Get("cam-1")
	if !ok {
		t.Fatal("camera not registered")
	}
	if url, _, ok := src.RTSPURLOf("cam-1"); !ok || !strings.HasSuffix(url, "/alias-a") {
		t.Fatalf("url=%q ok=%v", url, ok)
	}

	// Unchanged bootstrap: no churn.
	n := notified.Load()
	if _, err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if s, _ := reg.Get("cam-1"); s != first {
		t.Fatal("stream replaced without a URL change")
	}
	if notified.Load() != n {
		t.Fatal("notified without a change")
	}

	// New alias (RTSP toggled off/on in Protect).
	f.update(func(f *fakeController) { f.alias = "alias-b" })
	if _, err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if s, _ := reg.Get("cam-1"); s == first {
		t.Fatal("stream not re-registered after URL change")
	}
	if url, _, _ := src.RTSPURLOf("cam-1"); !strings.HasSuffix(url, "/alias-b") {
		t.Fatalf("url not updated: %q", url)
	}
	if notified.Load() == n {
		t.Fatal("URL change not notified")
	}

	// RTSP disabled on every channel.
	f.update(func(f *fakeController) { f.rtspOn = false })
	if _, err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get("cam-1"); ok {
		t.Fatal("stream still registered after RTSP disabled")
	}
	if _, _, ok := src.RTSPURLOf("cam-1"); ok {
		t.Fatal("RTSP URL still resolvable after RTSP disabled")
	}

	// Re-enabled, then the camera is removed from Protect.
	f.update(func(f *fakeController) { f.rtspOn = true })
	if _, err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	f.update(func(f *fakeController) { f.present = false })
	if _, err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get("cam-1"); ok {
		t.Fatal("stream still registered after camera removed")
	}
	if _, _, ok := src.RTSPURLOf("cam-1"); ok {
		t.Fatal("RTSP URL still resolvable after camera removed")
	}
}

// TestRefreshSerialized locks finding #3: concurrent Refresh calls must
// not overlap.
func TestRefreshSerialized(t *testing.T) {
	f := newFakeController(t)
	r, _, _ := newTestReconciler(t, f)
	if _, err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	f.update(func(f *fakeController) { f.gate = gate })

	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			if _, err := r.Refresh(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	// Give unserialized refreshes ample time to pile up in the handler
	// before releasing them; serialized ones never get past one.
	for deadline := time.Now().Add(300 * time.Millisecond); time.Now().Before(deadline) && f.inFlight.Load() < 2; {
		time.Sleep(time.Millisecond)
	}
	close(gate)
	wg.Wait()
	if got := f.maxSeen.Load(); got != 1 {
		t.Fatalf("max concurrent bootstrap fetches = %d, want 1", got)
	}
}

// TestRefreshKeepsNewerWSOnlineState locks the stale-snapshot half of
// finding #3: a bootstrap fetched before a WS offline event must not flip
// the camera back online.
func TestRefreshKeepsNewerWSOnlineState(t *testing.T) {
	f := newFakeController(t)
	r, _, src := newTestReconciler(t, f)
	if _, err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	f.update(func(f *fakeController) { f.gate = gate })

	done := make(chan error, 1)
	go func() {
		_, err := r.Refresh(context.Background())
		done <- err
	}()
	// Wait until the bootstrap request is parked in the handler.
	for f.inFlight.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	// The camera drops while the (CONNECTED) bootstrap is in flight.
	r.handleStateChange("cam-1", false)
	gate <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if src.snapshotMap()["cam-1"].Online {
		t.Fatal("stale bootstrap overwrote newer WS offline state")
	}

	// A later bootstrap that postdates the event is authoritative again.
	f.update(func(f *fakeController) { f.gate = nil })
	if _, err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !src.snapshotMap()["cam-1"].Online {
		t.Fatal("fresh bootstrap did not restore online state")
	}
}

// TestSetAllowedNotifiesStreams locks finding #1 at the source: editing
// the allow-list must notify the stream servers so they can prune.
func TestSetAllowedNotifiesStreams(t *testing.T) {
	src := NewCameraSource(nil)
	src.setRTSPURL("cam-1", "rtsps://h/a", false)
	var notified atomic.Int32
	src.OnStreamsChanged(func() { notified.Add(1) })
	src.SetAllowed([]string{"cam-2"})
	if notified.Load() != 1 {
		t.Fatalf("notified=%d want 1", notified.Load())
	}
	if _, _, ok := src.RTSPURLOf("cam-1"); ok {
		t.Fatal("hidden camera still resolvable")
	}
}
