package unifi

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/config"
	"github.com/gorilla/websocket"
)

// fakeProtect is an in-process stand-in for a UniFi Protect controller that
// exposes the three endpoints we exercise: login, bootstrap, and the updates WS.
type fakeProtect struct {
	srv *httptest.Server
	up  websocket.Upgrader

	mu      sync.Mutex
	clients map[*websocket.Conn]struct{}

	bootstrap bootstrapJSON
}

func newFakeProtect(t *testing.T) *fakeProtect {
	t.Helper()
	f := &fakeProtect{
		clients: map[*websocket.Conn]struct{}{},
		up:      websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
		bootstrap: bootstrapJSON{
			LastUpdateID: "u-0",
			Cameras: []cameraJSON{
				{
					ID:       "cam-1",
					Name:     "Front Door",
					MAC:      "aa:bb:cc:dd:ee:01",
					ModelKey: "camera",
					Type:     "G4 Doorbell Pro",
					State:    "CONNECTED",
					Channels: []channelJSON{{
						ID: 0, Name: "High", Width: 1600, Height: 1200,
						FPS: 30, Bitrate: 4000000,
						RTSPAlias: "abc123", IsRTSPEnabled: true,
					}},
				},
			},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-CSRF-Token", "test-csrf")
		http.SetCookie(w, &http.Cookie{Name: "TOKEN", Value: "session-xyz", Path: "/"})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/proxy/protect/api/bootstrap", func(w http.ResponseWriter, r *http.Request) {
		// Reject if the session cookie is missing — proves the client carried it.
		if _, err := r.Cookie("TOKEN"); err != nil {
			http.Error(w, "no session", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.bootstrap)
	})
	mux.HandleFunc("/proxy/protect/ws/updates", func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("TOKEN"); err != nil {
			http.Error(w, "no session", http.StatusUnauthorized)
			return
		}
		c, err := f.up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.clients[c] = struct{}{}
		f.mu.Unlock()
	})

	f.srv = httptest.NewTLSServer(mux)
	return f
}

func (f *fakeProtect) close() {
	f.mu.Lock()
	for c := range f.clients {
		_ = c.Close()
	}
	f.mu.Unlock()
	f.srv.Close()
}

// pushEvent encodes an action+data pair as a Protect binary frame and writes
// it to every connected WS client.
func (f *fakeProtect) pushEvent(t *testing.T, action wsAction, data any, deflate bool) {
	t.Helper()
	actBytes, err := json.Marshal(action)
	if err != nil {
		t.Fatalf("marshal action: %v", err)
	}
	var dataBytes []byte
	if data != nil {
		dataBytes, err = json.Marshal(data)
		if err != nil {
			t.Fatalf("marshal data: %v", err)
		}
	}
	frame := append(packFrame(1, deflate, actBytes), packFrame(2, deflate, dataBytes)...)

	f.mu.Lock()
	defer f.mu.Unlock()
	for c := range f.clients {
		if err := c.WriteMessage(websocket.BinaryMessage, frame); err != nil {
			t.Fatalf("ws write: %v", err)
		}
	}
}

func packFrame(pktType byte, deflated bool, payload []byte) []byte {
	body := payload
	if deflated {
		var buf bytes.Buffer
		zw := zlib.NewWriter(&buf)
		_, _ = zw.Write(payload)
		_ = zw.Close()
		body = buf.Bytes()
	}
	var hdr [8]byte
	hdr[0] = pktType
	hdr[1] = 1
	if deflated {
		hdr[2] = 1
	}
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(body)))
	out := make([]byte, 0, 8+len(body))
	out = append(out, hdr[:]...)
	out = append(out, body...)
	return out
}

func TestClient_LoginBootstrapEvents(t *testing.T) {
	f := newFakeProtect(t)
	defer f.close()

	u, _ := url.Parse(f.srv.URL)
	c := New(config.UniFi{
		Host:      u.Host,
		Username:  "x",
		Password:  "y",
		VerifyTLS: false,
	})
	// Trust the httptest TLS cert via the test server's helper.
	c.http.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.Login(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}

	cams, last, err := c.Bootstrap(ctx)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if len(cams) != 1 || cams[0].ID != "cam-1" || !cams[0].Online {
		t.Fatalf("unexpected cameras: %+v", cams)
	}
	if last != "u-0" {
		t.Fatalf("lastUpdateId: %q", last)
	}

	// Subscribe and verify two events round-trip.
	ch, err := c.SubscribeEvents(ctx, last)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Give the WS time to register on the server side.
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		n := len(f.clients)
		f.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server never saw ws client")
		}
		time.Sleep(10 * time.Millisecond)
	}

	f.pushEvent(t, wsAction{
		Action: "update", NewUpdateID: "u-1", ModelKey: "camera", ID: "cam-1",
	}, map[string]any{"state": "DISCONNECTED"}, false)

	f.pushEvent(t, wsAction{
		Action: "add", NewUpdateID: "u-2", ModelKey: "camera", ID: "cam-2",
	}, map[string]any{"id": "cam-2", "name": "Backyard", "state": "CONNECTED"}, true)

	got := make([]Event, 0, 2)
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for len(got) < 2 {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed early; got=%v", got)
			}
			got = append(got, ev)
		case <-timer.C:
			t.Fatalf("timed out; got=%v", got)
		}
	}

	if got[0].ID != "cam-1" || got[0].Action != "update" || got[0].NewUpdateID != "u-1" {
		t.Fatalf("event[0]: %+v", got[0])
	}
	if !strings.Contains(string(got[0].Fields["state"]), "DISCONNECTED") {
		t.Fatalf("event[0] fields: %v", got[0].Fields)
	}
	if got[1].ID != "cam-2" || got[1].Action != "add" || got[1].NewUpdateID != "u-2" {
		t.Fatalf("event[1]: %+v", got[1])
	}
	if !strings.Contains(string(got[1].Fields["name"]), "Backyard") {
		t.Fatalf("event[1] fields: %v", got[1].Fields)
	}
}
