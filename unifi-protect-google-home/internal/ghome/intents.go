// Cloud-to-Cloud integration model.
//
// Intents handled:
//
//	action.devices.SYNC    — list cameras as action.devices.types.CAMERA
//	                         with trait action.devices.traits.CameraStream
//	action.devices.QUERY   — return online status
//	action.devices.EXECUTE — action.devices.commands.GetCameraStream:
//	                         returns cameraStreamSignalingUrl, ICE servers,
//	                         cameraStreamProtocol="webrtc"
//
// Report State to https://homegraph.googleapis.com/v1/devices:reportStateAndNotification
// is used when cameras are added/removed/online state changes.
package ghome

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strings"
)

// Camera is what fulfillment needs to know about each device.
type Camera struct {
	ID           string
	Name         string
	Manufacturer string
	Model        string
	Online       bool
}

// IsDoorbell reports whether the camera should be exposed as a Google Home
// DOORBELL device (renders as a doorbell tile and can emit ring
// notifications via the ObjectDetection trait).
func IsDoorbell(c Camera) bool {
	return strings.Contains(strings.ToLower(c.Model), "doorbell") ||
		strings.Contains(strings.ToLower(c.Name), "doorbell")
}

// Source supplies fulfillment with the current camera list and per-request
// signaling URLs.
type Source interface {
	ListCameras() []Camera
	// SignalingURL returns an absolute URL Google will POST the SDP offer to.
	// Implementations should embed a short-lived signed token.
	SignalingURL(camID string) (url string, err error)
	// HLSURL returns the camera's HLS playlist URL. The Android Home app on
	// phones renders camera tiles only via HLS, so this is the protocol the
	// phone surface will pick. The URL embeds a path-scoped HMAC token, so
	// no Authorization header is required.
	HLSURL(camID string) (url string, err error)
	// ProgressiveMP4URL returns the camera's progressive_mp4 stream URL and
	// the bearer token Google should send as the Authorization header for
	// `cameraStreamAuthToken`.
	ProgressiveMP4URL(camID string) (url string, token string, err error)
}

// Handler is the HTTP entrypoint for the Smart Home Action fulfillment URL.
type Handler struct {
	Source Source
	// AgentUserID identifies this bridge instance to Google. It MUST match
	// the ID used for HomeGraph RequestSync/ReportState calls, or Google
	// files the devices under one user while state reports target another
	// (persistent "agent user not found" 404s). Empty falls back to the
	// historical default.
	AgentUserID string
}

func (h *Handler) agentUserID() string {
	if h.AgentUserID != "" {
		return h.AgentUserID
	}
	return "unifi-protect-bridge"
}

// ServeHTTP dispatches the Smart Home intent.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req intentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if len(req.Inputs) == 0 {
		http.Error(w, "no inputs", http.StatusBadRequest)
		return
	}
	resp := h.handle(req)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *Handler) handle(req intentRequest) intentResponse {
	in := req.Inputs[0]
	log.Printf("ghome intent: %s (reqID=%s)", in.Intent, req.RequestID)
	switch in.Intent {
	case "action.devices.SYNC":
		return h.sync(req.RequestID)
	case "action.devices.QUERY":
		return h.query(req.RequestID, in.Payload)
	case "action.devices.EXECUTE":
		return h.execute(req.RequestID, in.Payload)
	case "action.devices.DISCONNECT":
		// User unlinked the integration in the Google Home app. Google
		// expects a 200 with an empty body; we have no per-user state to
		// clear because OAuth tokens are stateless.
		return intentResponse{RequestID: req.RequestID, Payload: map[string]any{}}
	default:
		return intentResponse{RequestID: req.RequestID}
	}
}

func (h *Handler) sync(reqID string) intentResponse {
	devices, protocols := h.buildSyncDevices()
	log.Printf("ghome sync: returning %d device(s) cameraStreamSupportedProtocols=%v", len(devices), protocols)
	return intentResponse{
		RequestID: reqID,
		Payload: map[string]any{
			"agentUserId": h.agentUserID(),
			"devices":     devices,
		},
	}
}

// buildSyncDevices materialises the SYNC device list without any logging
// side effects, so it can be called both from sync() (real intent) and
// from SyncFingerprint() (offline fingerprint).
func (h *Handler) buildSyncDevices() ([]device, []string) {
	cams := h.Source.ListCameras()
	devices := make([]device, 0, len(cams))
	// Advertise the three protocols Google's surfaces actually consume:
	//   - hls           : Android phone Home app live-view
	//   - progressive_mp4: Chromecast / Nest Hub (non-Max) Cast playback
	//   - webrtc        : Nest Hub Max
	// Google picks one per surface based on which protocols that surface
	// supports; the bridge implements all three.
	protocols := []string{"hls", "progressive_mp4", "webrtc"}
	for _, c := range cams {
		devType := "action.devices.types.CAMERA"
		traits := []string{"action.devices.traits.CameraStream"}
		if IsDoorbell(c) {
			devType = "action.devices.types.DOORBELL"
			traits = append(traits, "action.devices.traits.ObjectDetection")
		}
		devices = append(devices, device{
			ID:                           c.ID,
			Type:                         devType,
			Traits:                       traits,
			Name:                         deviceName{Name: c.Name},
			WillReportState:              true,
			NotificationSupportedByAgent: true,
			Attributes: map[string]any{
				"cameraStreamSupportedProtocols": protocols,
				"cameraStreamNeedAuthToken":      true,
				"cameraStreamNeedDrmEncryption":  false,
			},
			DeviceInfo: &deviceInfo{
				Manufacturer: c.Manufacturer,
				Model:        c.Model,
			},
		})
	}
	return devices, protocols
}

// SyncFingerprint returns a stable hex SHA-256 over the current SYNC
// device list, suitable for deciding whether to issue HomeGraph
// RequestSync on bridge startup.
//
// Anything that affects the SYNC response — camera ID/name/manufacturer/
// model set, doorbell trait, advertised protocol list, auth flags —
// naturally changes the fingerprint. The bridge version itself does not
// need to be mixed in because every capability change is already reflected
// in the marshalled device slice.
//
// Devices are sorted by ID for determinism; Go's encoding/json already
// emits map keys in sorted order, so Attributes is stable too.
func (h *Handler) SyncFingerprint() string {
	devs, _ := h.buildSyncDevices()
	sorted := make([]device, len(devs))
	copy(sorted, devs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	// The agent user ID is part of the SYNC response, so a change to it
	// must change the fingerprint and re-trigger the startup RequestSync.
	b, _ := json.Marshal(struct {
		AgentUserID string   `json:"agentUserId"`
		Devices     []device `json:"devices"`
	}{h.agentUserID(), sorted})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (h *Handler) query(reqID string, payload json.RawMessage) intentResponse {
	var p queryPayload
	_ = json.Unmarshal(payload, &p)
	devices := map[string]any{}
	online := map[string]bool{}
	for _, c := range h.Source.ListCameras() {
		online[c.ID] = c.Online
	}
	results := make([]string, 0, len(p.Devices))
	for _, d := range p.Devices {
		on := online[d.ID]
		if on {
			results = append(results, d.ID+"=online")
		} else {
			results = append(results, d.ID+"=OFFLINE")
		}
		devices[d.ID] = map[string]any{
			"online": on,
			"status": "SUCCESS",
		}
	}
	log.Printf("ghome query: %d device(s) %v", len(p.Devices), results)
	return intentResponse{
		RequestID: reqID,
		Payload:   map[string]any{"devices": devices},
	}
}

func (h *Handler) execute(reqID string, payload json.RawMessage) intentResponse {
	var p executePayload
	_ = json.Unmarshal(payload, &p)
	commands := []map[string]any{}
	for _, cmd := range p.Commands {
		for _, exec := range cmd.Execution {
			if exec.Command != "action.devices.commands.GetCameraStream" {
				continue
			}
			supported := extractSupportedProtocols(exec.Params)
			ids := make([]string, 0, len(cmd.Devices))
			for _, d := range cmd.Devices {
				ids = append(ids, d.ID)
			}
			log.Printf("ghome execute: GetCameraStream devices=%v SupportedStreamProtocols=%v", ids, supported)

			// Protocol selection rules, in priority order:
			//   1. The supported list contains only "webrtc"     -> webrtc
			//      (this is exclusively Nest Hub Max).
			//   2. The supported list contains "hls"             -> hls
			//      (Android phone Home app — the only protocol it actually
			//      renders for cloud-to-cloud cameras).
			//   3. The supported list contains "progressive_mp4" -> mp4
			//      (Chromecast / Nest Hub non-Max Cast receivers).
			// Otherwise we return functionNotSupported.
			useWebRTC := len(supported) == 1 && containsFold(supported, "webrtc")
			useHLS := !useWebRTC && containsFold(supported, "hls")
			useMP4 := !useWebRTC && !useHLS && (len(supported) == 0 || containsFold(supported, "progressive_mp4"))

			for _, d := range cmd.Devices {
				switch {
				case useWebRTC:
					url, err := h.Source.SignalingURL(d.ID)
					if err != nil {
						commands = append(commands, deviceErr(d.ID, "deviceOffline"))
						continue
					}
					commands = append(commands, map[string]any{
						"ids":    []string{d.ID},
						"status": "SUCCESS",
						"states": map[string]any{
							"online":                   true,
							"cameraStreamProtocol":     "webrtc",
							"cameraStreamSignalingUrl": url,
							"cameraStreamAuthToken":    "",
						},
					})
				case useHLS:
					url, err := h.Source.HLSURL(d.ID)
					if err != nil {
						commands = append(commands, deviceErr(d.ID, "deviceOffline"))
						continue
					}
					commands = append(commands, map[string]any{
						"ids":    []string{d.ID},
						"status": "SUCCESS",
						"states": map[string]any{
							"online":                true,
							"cameraStreamProtocol":  "hls",
							"cameraStreamAccessUrl": url,
							// Path tokens self-authenticate; we still set a
							// placeholder since cameraStreamNeedAuthToken=true.
							"cameraStreamAuthToken": "hls",
						},
					})
				case useMP4:
					url, tok, err := h.Source.ProgressiveMP4URL(d.ID)
					if err != nil {
						commands = append(commands, deviceErr(d.ID, "deviceOffline"))
						continue
					}
					commands = append(commands, map[string]any{
						"ids":    []string{d.ID},
						"status": "SUCCESS",
						"states": map[string]any{
							"online":                    true,
							"cameraStreamProtocol":      "progressive_mp4",
							"cameraStreamAccessUrl":     url,
							"cameraStreamAuthToken":     tok,
							"cameraStreamReceiverAppId": "00F7C5DD",
						},
					})
				default:
					log.Printf("ghome execute: camera %s requested protocols %v — no overlap with [hls,webrtc,progressive_mp4], returning functionNotSupported", d.ID, supported)
					commands = append(commands, deviceErr(d.ID, "functionNotSupported"))
				}
			}
		}
	}
	return intentResponse{
		RequestID: reqID,
		Payload:   map[string]any{"commands": commands},
	}
}

func deviceErr(id, code string) map[string]any {
	return map[string]any{
		"ids":       []string{id},
		"status":    "ERROR",
		"errorCode": code,
	}
}

func extractSupportedProtocols(params map[string]any) []string {
	if params == nil {
		return nil
	}
	raw, ok := params["SupportedStreamProtocols"]
	if !ok {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func containsFold(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.EqualFold(s, needle) {
			return true
		}
	}
	return false
}

// --- intent JSON shapes ---

type intentRequest struct {
	RequestID string  `json:"requestId"`
	Inputs    []input `json:"inputs"`
}

type input struct {
	Intent  string          `json:"intent"`
	Payload json.RawMessage `json:"payload"`
}

type intentResponse struct {
	RequestID string         `json:"requestId"`
	Payload   map[string]any `json:"payload"`
}

type device struct {
	ID                           string         `json:"id"`
	Type                         string         `json:"type"`
	Traits                       []string       `json:"traits"`
	Name                         deviceName     `json:"name"`
	WillReportState              bool           `json:"willReportState"`
	NotificationSupportedByAgent bool           `json:"notificationSupportedByAgent,omitempty"`
	Attributes                   map[string]any `json:"attributes,omitempty"`
	DeviceInfo                   *deviceInfo    `json:"deviceInfo,omitempty"`
}

type deviceName struct {
	Name string `json:"name"`
}

type deviceInfo struct {
	Manufacturer string `json:"manufacturer,omitempty"`
	Model        string `json:"model,omitempty"`
}

type queryPayload struct {
	Devices []struct {
		ID string `json:"id"`
	} `json:"devices"`
}

type executePayload struct {
	Commands []struct {
		Devices []struct {
			ID string `json:"id"`
		} `json:"devices"`
		Execution []struct {
			Command string         `json:"command"`
			Params  map[string]any `json:"params"`
		} `json:"execution"`
	} `json:"commands"`
}
