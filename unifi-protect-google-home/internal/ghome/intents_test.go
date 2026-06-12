package ghome

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSource is a configurable Source for driving Handler tests.
type fakeSource struct {
	cams []Camera

	signalURL string
	signalErr error
	hlsURL    string
	hlsErr    error
	mp4URL    string
	mp4Token  string
	mp4Err    error
}

func newFakeSource(cams ...Camera) *fakeSource {
	return &fakeSource{ //nolint:gosec // fixture URLs/token, not credentials
		cams:      cams,
		signalURL: "https://bridge.example/webrtc/signal/",
		hlsURL:    "https://bridge.example/hls/index.m3u8?cam=",
		mp4URL:    "https://bridge.example/mp4/stream.mp4?cam=",
		mp4Token:  "mp4-bearer-token",
	}
}

func (f *fakeSource) ListCameras() []Camera { return f.cams }

func (f *fakeSource) SignalingURL(camID string) (string, error) {
	if f.signalErr != nil {
		return "", f.signalErr
	}
	return f.signalURL + camID, nil
}

func (f *fakeSource) HLSURL(camID string) (string, error) {
	if f.hlsErr != nil {
		return "", f.hlsErr
	}
	return f.hlsURL + camID, nil
}

func (f *fakeSource) ProgressiveMP4URL(camID string) (string, string, error) {
	if f.mp4Err != nil {
		return "", "", f.mp4Err
	}
	return f.mp4URL + camID, f.mp4Token, nil
}

// doIntent posts a raw intent JSON body through ServeHTTP and decodes the
// response envelope.
func doIntent(t *testing.T, h *Handler, body string) (map[string]any, int) {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/fulfillment", strings.NewReader(body)))
	if rr.Code != http.StatusOK {
		return nil, rr.Code
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, rr.Body.String())
	}
	return resp, rr.Code
}

func mustMap(t *testing.T, v any, what string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s: expected JSON object, got %T (%v)", what, v, v)
	}
	return m
}

func mustSlice(t *testing.T, v any, what string) []any {
	t.Helper()
	s, ok := v.([]any)
	if !ok {
		t.Fatalf("%s: expected JSON array, got %T (%v)", what, v, v)
	}
	return s
}

func strSlice(t *testing.T, v any, what string) []string {
	t.Helper()
	out := []string{}
	for _, e := range mustSlice(t, v, what) {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("%s: expected string element, got %T", what, e)
		}
		out = append(out, s)
	}
	return out
}

func containsStr(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// --- SYNC ---

func TestSyncDeviceList(t *testing.T) {
	src := newFakeSource(
		Camera{ID: "cam1", Name: "Driveway", Manufacturer: "Ubiquiti", Model: "G5 Bullet", Online: true},
		Camera{ID: "cam2", Name: "Front Door", Manufacturer: "Ubiquiti", Model: "G4 Doorbell Pro", Online: true},
	)
	h := &Handler{Source: src, AgentUserID: "agent-42"}

	resp, code := doIntent(t, h, `{"requestId":"req-1","inputs":[{"intent":"action.devices.SYNC"}]}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp["requestId"] != "req-1" {
		t.Fatalf("requestId = %v, want req-1", resp["requestId"])
	}
	payload := mustMap(t, resp["payload"], "payload")
	if payload["agentUserId"] != "agent-42" {
		t.Fatalf("agentUserId = %v, want agent-42", payload["agentUserId"])
	}
	devices := mustSlice(t, payload["devices"], "devices")
	if len(devices) != 2 {
		t.Fatalf("got %d devices, want 2", len(devices))
	}

	// Plain camera.
	cam := mustMap(t, devices[0], "devices[0]")
	if cam["id"] != "cam1" {
		t.Fatalf("devices[0].id = %v, want cam1", cam["id"])
	}
	if cam["type"] != "action.devices.types.CAMERA" {
		t.Fatalf("devices[0].type = %v, want CAMERA", cam["type"])
	}
	traits := strSlice(t, cam["traits"], "devices[0].traits")
	if !containsStr(traits, "action.devices.traits.CameraStream") {
		t.Fatalf("devices[0].traits = %v, missing CameraStream", traits)
	}
	if containsStr(traits, "action.devices.traits.ObjectDetection") {
		t.Fatalf("devices[0].traits = %v, plain camera must not have ObjectDetection", traits)
	}
	if name := mustMap(t, cam["name"], "devices[0].name"); name["name"] != "Driveway" {
		t.Fatalf("devices[0].name.name = %v, want Driveway", name["name"])
	}
	if cam["willReportState"] != true {
		t.Fatalf("devices[0].willReportState = %v, want true", cam["willReportState"])
	}
	info := mustMap(t, cam["deviceInfo"], "devices[0].deviceInfo")
	if info["manufacturer"] != "Ubiquiti" || info["model"] != "G5 Bullet" {
		t.Fatalf("devices[0].deviceInfo = %v", info)
	}
	attrs := mustMap(t, cam["attributes"], "devices[0].attributes")
	protocols := strSlice(t, attrs["cameraStreamSupportedProtocols"], "cameraStreamSupportedProtocols")
	want := []string{"hls", "progressive_mp4", "webrtc"}
	if len(protocols) != len(want) {
		t.Fatalf("cameraStreamSupportedProtocols = %v, want %v", protocols, want)
	}
	for i := range want {
		if protocols[i] != want[i] {
			t.Fatalf("cameraStreamSupportedProtocols = %v, want %v", protocols, want)
		}
	}
	if attrs["cameraStreamNeedAuthToken"] != true {
		t.Fatalf("cameraStreamNeedAuthToken = %v, want true", attrs["cameraStreamNeedAuthToken"])
	}
	if attrs["cameraStreamNeedDrmEncryption"] != false {
		t.Fatalf("cameraStreamNeedDrmEncryption = %v, want false", attrs["cameraStreamNeedDrmEncryption"])
	}

	// Doorbell (by model) gets the DOORBELL type plus ObjectDetection.
	door := mustMap(t, devices[1], "devices[1]")
	if door["type"] != "action.devices.types.DOORBELL" {
		t.Fatalf("devices[1].type = %v, want DOORBELL", door["type"])
	}
	doorTraits := strSlice(t, door["traits"], "devices[1].traits")
	if !containsStr(doorTraits, "action.devices.traits.CameraStream") ||
		!containsStr(doorTraits, "action.devices.traits.ObjectDetection") {
		t.Fatalf("devices[1].traits = %v, want CameraStream+ObjectDetection", doorTraits)
	}
}

func TestSyncDefaultAgentUserID(t *testing.T) {
	h := &Handler{Source: newFakeSource()}
	resp, _ := doIntent(t, h, `{"requestId":"r","inputs":[{"intent":"action.devices.SYNC"}]}`)
	payload := mustMap(t, resp["payload"], "payload")
	if payload["agentUserId"] != "unifi-protect-bridge" {
		t.Fatalf("agentUserId = %v, want unifi-protect-bridge", payload["agentUserId"])
	}
}

func TestIsDoorbell(t *testing.T) {
	cases := []struct {
		name string
		cam  Camera
		want bool
	}{
		{"by model", Camera{Name: "Entry", Model: "G4 Doorbell Pro"}, true},
		{"by name", Camera{Name: "Side Doorbell", Model: "G5 Bullet"}, true},
		{"case insensitive", Camera{Name: "FRONT DOORBELL", Model: ""}, true},
		{"plain camera", Camera{Name: "Driveway", Model: "G5 Bullet"}, false},
	}
	for _, tc := range cases {
		if got := IsDoorbell(tc.cam); got != tc.want {
			t.Errorf("%s: IsDoorbell(%+v) = %v, want %v", tc.name, tc.cam, got, tc.want)
		}
	}
}

// --- SyncFingerprint ---

func TestSyncFingerprintStable(t *testing.T) {
	h := &Handler{Source: newFakeSource(
		Camera{ID: "cam1", Name: "Driveway", Model: "G5 Bullet"},
	)}
	if a, b := h.SyncFingerprint(), h.SyncFingerprint(); a != b {
		t.Fatalf("fingerprint not stable across calls: %s != %s", a, b)
	}
}

func TestSyncFingerprintChangesOnCameraAdded(t *testing.T) {
	src := newFakeSource(Camera{ID: "cam1", Name: "Driveway"})
	h := &Handler{Source: src}
	before := h.SyncFingerprint()
	src.cams = append(src.cams, Camera{ID: "cam2", Name: "Backyard"})
	if after := h.SyncFingerprint(); after == before {
		t.Fatal("fingerprint unchanged after adding a camera")
	}
}

func TestSyncFingerprintChangesOnRename(t *testing.T) {
	src := newFakeSource(Camera{ID: "cam1", Name: "Driveway"})
	h := &Handler{Source: src}
	before := h.SyncFingerprint()
	src.cams[0].Name = "Front Yard"
	if after := h.SyncFingerprint(); after == before {
		t.Fatal("fingerprint unchanged after renaming a camera")
	}
}

func TestSyncFingerprintChangesOnAgentUserID(t *testing.T) {
	src := newFakeSource(Camera{ID: "cam1", Name: "Driveway"})
	a := (&Handler{Source: src, AgentUserID: "agent-a"}).SyncFingerprint()
	b := (&Handler{Source: src, AgentUserID: "agent-b"}).SyncFingerprint()
	if a == b {
		t.Fatal("fingerprint unchanged after agent user ID change")
	}
	// Empty AgentUserID falls back to the historical default, so it must
	// fingerprint identically to the explicit default.
	def := (&Handler{Source: src}).SyncFingerprint()
	expl := (&Handler{Source: src, AgentUserID: "unifi-protect-bridge"}).SyncFingerprint()
	if def != expl {
		t.Fatalf("default fingerprint %s != explicit default %s", def, expl)
	}
}

func TestSyncFingerprintOrderInsensitive(t *testing.T) {
	cam1 := Camera{ID: "cam1", Name: "Driveway", Model: "G5 Bullet"}
	cam2 := Camera{ID: "cam2", Name: "Front Door", Model: "G4 Doorbell"}
	a := (&Handler{Source: newFakeSource(cam1, cam2)}).SyncFingerprint()
	b := (&Handler{Source: newFakeSource(cam2, cam1)}).SyncFingerprint()
	if a != b {
		t.Fatalf("fingerprint sensitive to camera order: %s != %s", a, b)
	}
}

// --- QUERY ---

func TestQueryOnlineStatus(t *testing.T) {
	h := &Handler{Source: newFakeSource(
		Camera{ID: "cam1", Name: "Driveway", Online: true},
		Camera{ID: "cam2", Name: "Backyard", Online: false},
	)}
	body := `{"requestId":"req-q","inputs":[{"intent":"action.devices.QUERY","payload":{"devices":[{"id":"cam1"},{"id":"cam2"},{"id":"ghost"}]}}]}`
	resp, code := doIntent(t, h, body)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	devices := mustMap(t, mustMap(t, resp["payload"], "payload")["devices"], "devices")
	wantOnline := map[string]bool{"cam1": true, "cam2": false, "ghost": false}
	if len(devices) != len(wantOnline) {
		t.Fatalf("got %d device results, want %d", len(devices), len(wantOnline))
	}
	for id, want := range wantOnline {
		d := mustMap(t, devices[id], "devices."+id)
		if d["online"] != want {
			t.Errorf("%s online = %v, want %v", id, d["online"], want)
		}
		if d["status"] != "SUCCESS" {
			t.Errorf("%s status = %v, want SUCCESS", id, d["status"])
		}
	}
}

// --- EXECUTE ---

// executeBody builds a GetCameraStream EXECUTE request. omitField leaves the
// SupportedStreamProtocols param out entirely (vs. an empty array).
func executeBody(t *testing.T, camID string, supported []string, omitField bool) string {
	t.Helper()
	params := map[string]any{}
	if !omitField {
		params["SupportedStreamProtocols"] = supported
	}
	req := map[string]any{
		"requestId": "req-x",
		"inputs": []any{map[string]any{
			"intent": "action.devices.EXECUTE",
			"payload": map[string]any{
				"commands": []any{map[string]any{
					"devices": []any{map[string]any{"id": camID}},
					"execution": []any{map[string]any{
						"command": "action.devices.commands.GetCameraStream",
						"params":  params,
					}},
				}},
			},
		}},
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal execute body: %v", err)
	}
	return string(b)
}

// firstCommand returns the single commands[] entry from an EXECUTE response.
func firstCommand(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	commands := mustSlice(t, mustMap(t, resp["payload"], "payload")["commands"], "commands")
	if len(commands) != 1 {
		t.Fatalf("got %d commands, want 1: %v", len(commands), commands)
	}
	cmd := mustMap(t, commands[0], "commands[0]")
	ids := strSlice(t, cmd["ids"], "commands[0].ids")
	if len(ids) != 1 || ids[0] != "cam1" {
		t.Fatalf("commands[0].ids = %v, want [cam1]", ids)
	}
	return cmd
}

func TestExecuteProtocolSelection(t *testing.T) {
	cases := []struct {
		name         string
		supported    []string
		omitField    bool
		wantProtocol string
	}{
		{"webrtc only", []string{"webrtc"}, false, "webrtc"},
		{"hls wins when present", []string{"hls", "dash", "smooth_stream", "progressive_mp4"}, false, "hls"},
		{"progressive_mp4 when webrtc not alone", []string{"progressive_mp4", "webrtc"}, false, "progressive_mp4"},
		{"empty list defaults to progressive_mp4", []string{}, false, "progressive_mp4"},
		{"missing field defaults to progressive_mp4", nil, true, "progressive_mp4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := newFakeSource(Camera{ID: "cam1", Name: "Driveway", Online: true})
			h := &Handler{Source: src}
			resp, code := doIntent(t, h, executeBody(t, "cam1", tc.supported, tc.omitField))
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200", code)
			}
			cmd := firstCommand(t, resp)
			if cmd["status"] != "SUCCESS" {
				t.Fatalf("status = %v, want SUCCESS: %v", cmd["status"], cmd)
			}
			states := mustMap(t, cmd["states"], "states")
			if states["cameraStreamProtocol"] != tc.wantProtocol {
				t.Fatalf("cameraStreamProtocol = %v, want %s", states["cameraStreamProtocol"], tc.wantProtocol)
			}
			if states["online"] != true {
				t.Fatalf("online = %v, want true", states["online"])
			}
			switch tc.wantProtocol {
			case "webrtc":
				if states["cameraStreamSignalingUrl"] != src.signalURL+"cam1" {
					t.Fatalf("cameraStreamSignalingUrl = %v, want %s", states["cameraStreamSignalingUrl"], src.signalURL+"cam1")
				}
			case "hls":
				if states["cameraStreamAccessUrl"] != src.hlsURL+"cam1" {
					t.Fatalf("cameraStreamAccessUrl = %v, want %s", states["cameraStreamAccessUrl"], src.hlsURL+"cam1")
				}
			case "progressive_mp4":
				if states["cameraStreamAccessUrl"] != src.mp4URL+"cam1" {
					t.Fatalf("cameraStreamAccessUrl = %v, want %s", states["cameraStreamAccessUrl"], src.mp4URL+"cam1")
				}
				if states["cameraStreamAuthToken"] != src.mp4Token {
					t.Fatalf("cameraStreamAuthToken = %v, want %s", states["cameraStreamAuthToken"], src.mp4Token)
				}
				if states["cameraStreamReceiverAppId"] != "00F7C5DD" {
					t.Fatalf("cameraStreamReceiverAppId = %v, want 00F7C5DD", states["cameraStreamReceiverAppId"])
				}
			}
		})
	}
}

func TestExecuteNoProtocolOverlap(t *testing.T) {
	h := &Handler{Source: newFakeSource(Camera{ID: "cam1", Name: "Driveway", Online: true})}
	resp, _ := doIntent(t, h, executeBody(t, "cam1", []string{"dash"}, false))
	cmd := firstCommand(t, resp)
	if cmd["status"] != "ERROR" {
		t.Fatalf("status = %v, want ERROR", cmd["status"])
	}
	if cmd["errorCode"] != "functionNotSupported" {
		t.Fatalf("errorCode = %v, want functionNotSupported", cmd["errorCode"])
	}
}

func TestExecuteSourceErrorsReportDeviceOffline(t *testing.T) {
	cases := []struct {
		name      string
		supported []string
		break_    func(*fakeSource)
	}{
		{"webrtc", []string{"webrtc"}, func(f *fakeSource) { f.signalErr = errors.New("nope") }},
		{"hls", []string{"hls"}, func(f *fakeSource) { f.hlsErr = errors.New("nope") }},
		{"progressive_mp4", []string{"progressive_mp4"}, func(f *fakeSource) { f.mp4Err = errors.New("nope") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := newFakeSource(Camera{ID: "cam1", Name: "Driveway", Online: true})
			tc.break_(src)
			h := &Handler{Source: src}
			resp, _ := doIntent(t, h, executeBody(t, "cam1", tc.supported, false))
			cmd := firstCommand(t, resp)
			if cmd["status"] != "ERROR" {
				t.Fatalf("status = %v, want ERROR", cmd["status"])
			}
			if cmd["errorCode"] != "deviceOffline" {
				t.Fatalf("errorCode = %v, want deviceOffline", cmd["errorCode"])
			}
		})
	}
}

// --- DISCONNECT / transport errors ---

func TestDisconnect(t *testing.T) {
	h := &Handler{Source: newFakeSource()}
	resp, code := doIntent(t, h, `{"requestId":"req-d","inputs":[{"intent":"action.devices.DISCONNECT"}]}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp["requestId"] != "req-d" {
		t.Fatalf("requestId = %v, want req-d", resp["requestId"])
	}
	payload := mustMap(t, resp["payload"], "payload")
	if len(payload) != 0 {
		t.Fatalf("payload = %v, want empty object", payload)
	}
}

func TestServeHTTPBadRequests(t *testing.T) {
	h := &Handler{Source: newFakeSource()}
	if _, code := doIntent(t, h, `{not json`); code != http.StatusBadRequest {
		t.Fatalf("bad json status = %d, want 400", code)
	}
	if _, code := doIntent(t, h, `{"requestId":"r","inputs":[]}`); code != http.StatusBadRequest {
		t.Fatalf("empty inputs status = %d, want 400", code)
	}
}
