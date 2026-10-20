package bridge

import (
	"errors"
	"sync"

	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/api"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/ghome"
)

// CameraSource is the ghome.Source implementation backed by an atomic
// snapshot of the latest bootstrap result.
type CameraSource struct {
	mu           sync.RWMutex
	cameras      []ghome.Camera
	rtspURLs     map[string]rtspEntry
	lastUpdateId string
	signaling    *api.Server
	// allowed is the set of camera IDs the user opted in to expose via
	// Google Home. nil means "allow all" (backward-compatible default for
	// installs without bridge.exposed_cameras configured).
	allowed map[string]bool
	// onlineGen[camID] is the value of gen when the camera's online flag
	// was last flipped by a live WS event. Refresh compares it against the
	// gen it sampled before fetching the bootstrap so a slow, stale
	// snapshot cannot undo a newer WS-observed transition.
	gen       uint64
	onlineGen map[string]uint64
	// streamsChanged is invoked (outside mu) whenever the allow-list or
	// the set of RTSP URLs changes, so the HLS/MP4 servers can drop
	// muxers that must no longer serve.
	streamsChanged func()
}

// NewCameraSource returns a CameraSource with the given exposure
// allow-list applied (empty/nil = all cameras).
func NewCameraSource(exposed []string) *CameraSource {
	s := &CameraSource{}
	s.SetAllowed(exposed)
	return s
}

// errCameraNotExposed is returned by the per-camera URL helpers when the
// camera ID is not in the configured exposure allow-list. Bubbling it out
// of Source.SignalingURL / HLSURL / ProgressiveMP4URL turns into a
// deviceOffline error in the ghome EXECUTE handler. RTSPURLOf applies the
// same gate at serving time, so even a stream URL issued just before the
// user trimmed the list stops working immediately.
var errCameraNotExposed = errors.New("camera is not exposed to google home")

// rtspEntry is the per-camera info the HLS muxer needs to open upstream.
type rtspEntry struct {
	URL       string
	VerifyTLS bool
}

// SetSignaling wires the api.Server the per-camera URL helpers delegate to.
func (s *CameraSource) SetSignaling(srv *api.Server) {
	s.mu.Lock()
	s.signaling = srv
	s.mu.Unlock()
}

func (s *CameraSource) signalingServer() *api.Server {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.signaling
}

func (s *CameraSource) lastUpdateID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastUpdateId
}

func (s *CameraSource) setLastUpdateID(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	s.lastUpdateId = id
	s.mu.Unlock()
}

func (s *CameraSource) set(cams []ghome.Camera) {
	s.mu.Lock()
	s.cameras = cams
	s.mu.Unlock()
}

// setOnline flips the cached online flag for a single camera. Returns
// (changed, camera, exists). When the camera is not in the current snapshot
// exists is false and the caller should fall back to a full bootstrap.
func (s *CameraSource) setOnline(camID string, online bool) (bool, ghome.Camera, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, c := range s.cameras {
		if c.ID != camID {
			continue
		}
		if c.Online == online {
			return false, c, true
		}
		s.cameras[i].Online = online
		s.gen++
		if s.onlineGen == nil {
			s.onlineGen = make(map[string]uint64)
		}
		s.onlineGen[camID] = s.gen
		return true, s.cameras[i], true
	}
	return false, ghome.Camera{}, false
}

// onlineGeneration returns the current WS state-change generation. Sample
// it before fetching a bootstrap and pass it to onlineChangedSince.
func (s *CameraSource) onlineGeneration() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.gen
}

// onlineChangedSince reports whether a WS event flipped camID's online
// flag after generation g was sampled.
func (s *CameraSource) onlineChangedSince(camID string, g uint64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.onlineGen[camID] > g
}

// OnStreamsChanged registers the callback fired whenever a camera's
// stream eligibility may have changed (allow-list edit, camera removed,
// RTSP URL changed). Set it once during wiring.
func (s *CameraSource) OnStreamsChanged(f func()) {
	s.mu.Lock()
	s.streamsChanged = f
	s.mu.Unlock()
}

func (s *CameraSource) notifyStreamsChanged() {
	s.mu.RLock()
	f := s.streamsChanged
	s.mu.RUnlock()
	if f != nil {
		f()
	}
}

// Snapshot returns a copy of the current camera list.
func (s *CameraSource) Snapshot() []ghome.Camera {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ghome.Camera, len(s.cameras))
	copy(out, s.cameras)
	return out
}

// SetAllowed installs the camera-exposure allow-list. ids==nil or an empty
// slice means "allow all cameras" (the historical behaviour). Subsequent
// ListCameras calls and signaling URL lookups honour the new set
// immediately, so the setup UI can hot-apply changes without restarting.
func (s *CameraSource) SetAllowed(ids []string) {
	var set map[string]bool
	for _, id := range ids {
		if id == "" {
			continue
		}
		if set == nil {
			set = make(map[string]bool, len(ids))
		}
		set[id] = true
	}
	s.mu.Lock()
	s.allowed = set
	s.mu.Unlock()
	s.notifyStreamsChanged()
}

// IsAllowed reports whether the camera is currently exposed to Google.
// A nil allow-list means "all cameras allowed".
func (s *CameraSource) IsAllowed(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.allowed == nil || s.allowed[id]
}

// snapshotMap returns the current cameras keyed by ID for diff-friendly lookup.
func (s *CameraSource) snapshotMap() map[string]ghome.Camera {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]ghome.Camera, len(s.cameras))
	for _, c := range s.cameras {
		out[c.ID] = c
	}
	return out
}

// ListCameras implements ghome.Source.
func (s *CameraSource) ListCameras() []ghome.Camera {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.allowed == nil {
		out := make([]ghome.Camera, len(s.cameras))
		copy(out, s.cameras)
		return out
	}
	out := make([]ghome.Camera, 0, len(s.cameras))
	for _, c := range s.cameras {
		if s.allowed[c.ID] {
			out = append(out, c)
		}
	}
	return out
}

// SignalingURL implements ghome.Source.
func (s *CameraSource) SignalingURL(camID string) (string, error) {
	if !s.IsAllowed(camID) {
		return "", errCameraNotExposed
	}
	return s.signalingServer().SignalingURL(camID)
}

// HLSURL implements ghome.Source.
func (s *CameraSource) HLSURL(camID string) (string, error) {
	if !s.IsAllowed(camID) {
		return "", errCameraNotExposed
	}
	return s.signalingServer().HLSURL(camID)
}

// ProgressiveMP4URL implements ghome.Source.
func (s *CameraSource) ProgressiveMP4URL(camID string) (string, string, error) {
	if !s.IsAllowed(camID) {
		return "", "", errCameraNotExposed
	}
	return s.signalingServer().ProgressiveMP4URL(camID)
}

// setRTSPURL records the RTSP url + TLS-verify flag for a camera so the
// HLS muxer can look it up by ID.
func (s *CameraSource) setRTSPURL(camID, url string, verifyTLS bool) {
	s.mu.Lock()
	if s.rtspURLs == nil {
		s.rtspURLs = make(map[string]rtspEntry)
	}
	s.rtspURLs[camID] = rtspEntry{URL: url, VerifyTLS: verifyTLS}
	s.mu.Unlock()
}

// deleteRTSPURL forgets a camera's RTSP URL (camera removed or its RTSP
// channel disabled).
func (s *CameraSource) deleteRTSPURL(camID string) {
	s.mu.Lock()
	delete(s.rtspURLs, camID)
	s.mu.Unlock()
}

// rtspURL returns the recorded RTSP URL for a camera regardless of the
// exposure allow-list.
func (s *CameraSource) rtspURL(camID string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.rtspURLs[camID]
	return e.URL, ok
}

// RTSPURLOf is the hls.Source/mp4.Source signature: returns the upstream
// RTSP URL and whether the upstream TLS certificate should be verified.
// Cameras outside the exposure allow-list resolve as unknown so that
// previously issued HLS/MP4 URLs (valid up to an hour) stop streaming the
// moment the user hides the camera.
func (s *CameraSource) RTSPURLOf(camID string) (string, bool, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.allowed != nil && !s.allowed[camID] {
		return "", false, false
	}
	e, ok := s.rtspURLs[camID]
	if !ok {
		return "", false, false
	}
	return e.URL, e.VerifyTLS, true
}
