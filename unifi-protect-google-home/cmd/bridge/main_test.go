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

// TestCameraSourceAllowList verifies bridge.exposed_cameras filtering:
// ListCameras hides disallowed cameras (so SYNC/QUERY do not advertise
// them) and the per-camera URL helpers refuse to mint stream URLs for
// cameras that are not in the allow-list. nil/empty allow-list means
// "all cameras allowed" for backward compatibility.
func TestCameraSourceAllowList(t *testing.T) {
	s := &cameraSource{}
	s.set([]ghome.Camera{
		{ID: "cam-1", Name: "Front", Online: true},
		{ID: "cam-2", Name: "Back", Online: true},
		{ID: "cam-3", Name: "Side", Online: true},
	})

	// Default (nil allow-list) exposes every camera.
	if got := len(s.ListCameras()); got != 3 {
		t.Fatalf("default: ListCameras=%d, want 3", got)
	}
	if !s.isAllowed("cam-1") || !s.isAllowed("cam-2") {
		t.Fatalf("default: every camera must report allowed")
	}

	// Allow-list of [cam-1, cam-3] hides cam-2.
	s.SetAllowed([]string{"cam-1", "cam-3"})
	cams := s.ListCameras()
	if len(cams) != 2 {
		t.Fatalf("filtered: ListCameras=%d, want 2: %+v", len(cams), cams)
	}
	for _, c := range cams {
		if c.ID == "cam-2" {
			t.Fatalf("cam-2 leaked through filter: %+v", cams)
		}
	}
	if s.isAllowed("cam-2") {
		t.Fatalf("cam-2 must be reported as not allowed")
	}

	// Stream URL helpers refuse disallowed cameras.
	if _, err := s.SignalingURL("cam-2"); err == nil {
		t.Fatalf("SignalingURL(cam-2): expected error for hidden camera")
	}
	if _, err := s.HLSURL("cam-2"); err == nil {
		t.Fatalf("HLSURL(cam-2): expected error for hidden camera")
	}
	if _, _, err := s.ProgressiveMP4URL("cam-2"); err == nil {
		t.Fatalf("ProgressiveMP4URL(cam-2): expected error for hidden camera")
	}

	// Empty list resets to allow-all.
	s.SetAllowed([]string{})
	if got := len(s.ListCameras()); got != 3 {
		t.Fatalf("reset: ListCameras=%d, want 3", got)
	}

	// All-empty-strings list also resets to allow-all (treated as unset).
	s.SetAllowed([]string{"", ""})
	if got := len(s.ListCameras()); got != 3 {
		t.Fatalf("blank-only: ListCameras=%d, want 3", got)
	}
}

// TestWSHasInterestingField verifies the bridge.ws_event_log "interesting"
// filter: only frames carrying a field the bridge actually reacts to
// should pass through, so high-frequency telemetry noise (uptime,
// lastSeen, stats, phyRate, nvrMac, …) is dropped.
func TestWSHasInterestingField(t *testing.T) {
	mk := func(keys ...string) map[string]json.RawMessage {
		m := map[string]json.RawMessage{}
		for _, k := range keys {
			m[k] = json.RawMessage("null")
		}
		return m
	}
	cases := []struct {
		name string
		in   map[string]json.RawMessage
		want bool
	}{
		{"telemetry only", mk("phyRate", "wifiConnectionState", "stats", "nvrMac"), false},
		{"uptime/lastSeen", mk("uptime", "lastSeen", "uplinkDevice"), false},
		{"motion", mk("isMotionDetected", "lastMotion", "nvrMac"), true},
		{"ring", mk("lastRing", "nvrMac"), true},
		{"connectivity state", mk("state", "uptime"), true},
		{"isConnected", mk("isConnected"), true},
		{"name change", mk("name"), true},
		{"channels change", mk("channels"), true},
		{"empty", mk(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := wsHasInterestingField(tc.in); got != tc.want {
				t.Fatalf("got=%v want=%v fields=%v", got, tc.want, tc.in)
			}
		})
	}
}

func TestWSLogLevelFromString(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int32
	}{
		{"off", wsLogOff},
		{"OFF", wsLogOff},
		{"interesting", wsLogInteresting},
		{"all", wsLogAll},
		{"", wsLogInteresting},
		{"bogus", wsLogInteresting},
	} {
		if got := wsLogLevelFromString(tc.in); got != tc.want {
			t.Fatalf("wsLogLevelFromString(%q)=%d want=%d", tc.in, got, tc.want)
		}
	}
}
