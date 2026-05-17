// Package unifi is a minimal client for the UniFi Protect controller.
//
// Auth flow (UniFi OS):
//  1. POST /api/auth/login   { username, password }   → sets TOKEN cookie + X-CSRF-Token header
//  2. GET  /proxy/protect/api/bootstrap               → cameras, NVR, lastUpdateId
//  3. GET  /proxy/protect/api/cameras/{id}/snapshot   → JPEG
//
// RTSPS URLs are constructed from each camera's channels[].rtspAlias:
//
//	rtsps://<host>:7441/<rtspAlias>
package unifi

// Camera is the minimal projection of a Protect camera we care about.
type Camera struct {
	ID         string
	Name       string
	MAC        string
	Model      string
	Type       string
	Online     bool
	HasMic     bool
	HasSpeaker bool
	Channels   []Channel
}

// Channel is a stream quality variant exposed by a Protect camera.
type Channel struct {
	ID            int
	Name          string // "High", "Medium", "Low"
	Width         int
	Height        int
	FPS           int
	Bitrate       int
	RTSPAlias     string // empty if RTSP is disabled for this channel
	IsRTSPEnabled bool
}

// BestRTSPChannel returns the highest-resolution channel that has RTSP enabled,
// or nil if none.
func (c Camera) BestRTSPChannel() *Channel {
	var best *Channel
	for i := range c.Channels {
		ch := &c.Channels[i]
		if !ch.IsRTSPEnabled || ch.RTSPAlias == "" {
			continue
		}
		if best == nil || ch.Width*ch.Height > best.Width*best.Height {
			best = ch
		}
	}
	return best
}
