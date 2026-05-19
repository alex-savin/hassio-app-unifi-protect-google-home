package main

import (
	"encoding/json"
	"testing"

	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/ghome"
)

// TestCameraSourceSetOnline locks the in-place online-flag update used by
// the WebSocket fast-path in handleStateChange. Both transitions must be
// reported as "changed" so the Test Suite OnlineOffline check observes
// online=false AND online=true within milliseconds of the Protect WS event.
func TestCameraSourceSetOnline(t *testing.T) {
	s := &cameraSource{}
	s.set([]ghome.Camera{
		{ID: "cam-1", Name: "Front", Online: true},
		{ID: "cam-2", Name: "Side", Online: true},
	})

	// online -> offline must report changed=true.
	changed, cam, ok := s.setOnline("cam-1", false)
	if !ok || !changed || cam.Online != false {
		t.Fatalf("offline transition: ok=%v changed=%v online=%v", ok, changed, cam.Online)
	}
	if got := s.snapshotMap()["cam-1"].Online; got != false {
		t.Fatalf("snapshot not updated after offline transition: online=%v", got)
	}

	// repeated offline is a no-op.
	if changed, _, _ := s.setOnline("cam-1", false); changed {
		t.Fatalf("idempotent offline call reported changed=true")
	}

	// offline -> online must report changed=true and update the snapshot.
	changed, cam, ok = s.setOnline("cam-1", true)
	if !ok || !changed || cam.Online != true {
		t.Fatalf("online transition: ok=%v changed=%v online=%v", ok, changed, cam.Online)
	}
	if got := s.snapshotMap()["cam-1"].Online; got != true {
		t.Fatalf("snapshot not updated after online transition: online=%v", got)
	}

	// unknown camera must be reported as exists=false so the caller can fall
	// back to a full bootstrap.
	if _, _, ok := s.setOnline("cam-unknown", true); ok {
		t.Fatalf("unknown camera reported as exists=true")
	}

	// other cameras must be untouched.
	if got := s.snapshotMap()["cam-2"].Online; got != true {
		t.Fatalf("cam-2 was mutated: online=%v", got)
	}
}

// TestHandleStateChangeNoHomeGraph ensures handleStateChange is safe to call
// when no HomeGraph client is configured (e.g. local-only dev mode) and
// still updates the snapshot so QUERY can return fresh data.
func TestHandleStateChangeNoHomeGraph(t *testing.T) {
	src := &cameraSource{}
	src.set([]ghome.Camera{{ID: "cam-1", Name: "Front", Online: true}})
	r := &reconciler{src: src, hg: nil}

	r.handleStateChange("cam-1", false)
	if got := src.snapshotMap()["cam-1"].Online; got != false {
		t.Fatalf("snapshot not updated when hg is nil: online=%v", got)
	}

	r.handleStateChange("cam-1", true)
	if got := src.snapshotMap()["cam-1"].Online; got != true {
		t.Fatalf("snapshot not restored to online: online=%v", got)
	}
}

// TestDecodeOnline covers the multiple shapes Protect can use to signal
// camera connectivity transitions over the updates WebSocket.
func TestDecodeOnline(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]json.RawMessage
		want   bool
		ok     bool
	}{
		{"state CONNECTED", map[string]json.RawMessage{"state": json.RawMessage(`"CONNECTED"`)}, true, true},
		{"state DISCONNECTED", map[string]json.RawMessage{"state": json.RawMessage(`"DISCONNECTED"`)}, false, true},
		{"state CONNECTING", map[string]json.RawMessage{"state": json.RawMessage(`"CONNECTING"`)}, false, true},
		{"isConnected true", map[string]json.RawMessage{"isConnected": json.RawMessage(`true`)}, true, true},
		{"isConnected false", map[string]json.RawMessage{"isConnected": json.RawMessage(`false`)}, false, true},
		{"unrelated field", map[string]json.RawMessage{"name": json.RawMessage(`"Front"`)}, false, false},
		{"empty", map[string]json.RawMessage{}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := decodeOnline(tc.fields)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("got=(%v,%v) want=(%v,%v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}
