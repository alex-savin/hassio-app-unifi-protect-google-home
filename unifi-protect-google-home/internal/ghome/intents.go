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
	"encoding/json"
	"net/http"
)

// Camera is what fulfillment needs to know about each device.
type Camera struct {
	ID           string
	Name         string
	Manufacturer string
	Model        string
	Online       bool
}

// Source supplies fulfillment with the current camera list and per-request
// signaling URLs.
type Source interface {
	ListCameras() []Camera
	// SignalingURL returns an absolute URL Google will POST the SDP offer to.
	// Implementations should embed a short-lived signed token.
	SignalingURL(camID string) (url string, err error)
}

// Handler is the HTTP entrypoint for the Smart Home Action fulfillment URL.
type Handler struct {
	Source Source
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
	switch in.Intent {
	case "action.devices.SYNC":
		return h.sync(req.RequestID)
	case "action.devices.QUERY":
		return h.query(req.RequestID, in.Payload)
	case "action.devices.EXECUTE":
		return h.execute(req.RequestID, in.Payload)
	default:
		return intentResponse{RequestID: req.RequestID}
	}
}

func (h *Handler) sync(reqID string) intentResponse {
	cams := h.Source.ListCameras()
	devices := make([]device, 0, len(cams))
	for _, c := range cams {
		devices = append(devices, device{
			ID:   c.ID,
			Type: "action.devices.types.CAMERA",
			Traits: []string{
				"action.devices.traits.CameraStream",
			},
			Name:            deviceName{Name: c.Name},
			WillReportState: true,
			Attributes: map[string]any{
				"cameraStreamSupportedProtocols": []string{"webrtc"},
				"cameraStreamNeedAuthToken":      false,
				"cameraStreamSupportsPreview":    false,
			},
			DeviceInfo: &deviceInfo{
				Manufacturer: c.Manufacturer,
				Model:        c.Model,
			},
		})
	}
	return intentResponse{
		RequestID: reqID,
		Payload: map[string]any{
			"agentUserId": "unifi-protect-bridge",
			"devices":     devices,
		},
	}
}

func (h *Handler) query(reqID string, payload json.RawMessage) intentResponse {
	var p queryPayload
	_ = json.Unmarshal(payload, &p)
	devices := map[string]any{}
	online := map[string]bool{}
	for _, c := range h.Source.ListCameras() {
		online[c.ID] = c.Online
	}
	for _, d := range p.Devices {
		devices[d.ID] = map[string]any{
			"online": online[d.ID],
			"status": "SUCCESS",
		}
	}
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
			for _, d := range cmd.Devices {
				url, err := h.Source.SignalingURL(d.ID)
				if err != nil {
					commands = append(commands, map[string]any{
						"ids":       []string{d.ID},
						"status":    "ERROR",
						"errorCode": "deviceOffline",
					})
					continue
				}
				commands = append(commands, map[string]any{
					"ids":    []string{d.ID},
					"status": "SUCCESS",
					"states": map[string]any{
						"online":                   true,
						"cameraStreamProtocol":     "webrtc",
						"cameraStreamSignalingUrl": url,
						"cameraStreamIceServers":   "",
					},
				})
			}
		}
	}
	return intentResponse{
		RequestID: reqID,
		Payload:   map[string]any{"commands": commands},
	}
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
	ID              string         `json:"id"`
	Type            string         `json:"type"`
	Traits          []string       `json:"traits"`
	Name            deviceName     `json:"name"`
	WillReportState bool           `json:"willReportState"`
	Attributes      map[string]any `json:"attributes,omitempty"`
	DeviceInfo      *deviceInfo    `json:"deviceInfo,omitempty"`
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
