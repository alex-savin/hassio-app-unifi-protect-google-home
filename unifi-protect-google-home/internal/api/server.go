// the optional admin endpoints.
package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/ghome"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/hls"
	mp4srv "github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/mp4"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/oauth"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/streams"
	wrtc "github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/webrtc"
)

// Server hosts every public HTTP endpoint. Everything mounted here is
// reachable from the internet (the user reverse-proxies this listener so
// Google can call the fulfillment URL), so each route must either be
// authenticated or carry an HMAC-signed token. LAN-only conveniences like
// the discovery scan live on the ingress-only setup server instead.
type Server struct {
	PublicBaseURL     string
	StreamTokenSecret []byte

	OAuth    *oauth.Server
	Fulfill  *ghome.Handler
	Registry *streams.Registry
	WebRTC   *wrtc.Factory
	HLS      *hls.Server
	MP4      *mp4srv.Server
}

// Routes returns an http.Handler with all endpoints mounted.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/authorize", s.OAuth.Authorize)
	mux.HandleFunc("/oauth/token", s.OAuth.Token)
	mux.Handle("/smarthome", s.authMiddleware(s.Fulfill))
	mux.HandleFunc("/webrtc/signal", s.signalHandler)
	if s.HLS != nil {
		mux.HandleFunc("/hls/", s.hlsHandler)
	}
	if s.MP4 != nil {
		mux.HandleFunc("/mp4/", s.mp4Handler)
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// authMiddleware validates the OAuth bearer token Google sends on fulfillment.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			http.Error(w, "missing bearer", http.StatusUnauthorized)
			return
		}
		tok := strings.TrimPrefix(h, "Bearer ")
		if err := s.OAuth.Validate(tok); err != nil {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// SignalingURL builds a signed, short-lived URL Google will POST the SDP offer to.
func (s *Server) SignalingURL(camID string) (string, error) {
	exp := time.Now().Add(2 * time.Minute).Unix()
	tok := s.signToken(camID, exp)
	return fmt.Sprintf("%s/webrtc/signal?cam=%s&exp=%d&t=%s",
		strings.TrimRight(s.PublicBaseURL, "/"), camID, exp, tok), nil
}

// HLSURL builds a signed, longer-lived URL pointing at the camera's HLS
// playlist. The expiry is generous (1 h) because mobile players keep a
// playlist URL around across reloads and a too-short window forces the
// Home app to refetch the EXECUTE response mid-playback.
//
// The token lives in the URL path (not the query string) so the relative
// segment URIs gohlslib emits inside the playlist automatically inherit
// it without any rewriting.
func (s *Server) HLSURL(camID string) (string, error) {
	exp := time.Now().Add(1 * time.Hour).Unix()
	tok := s.signToken(camID, exp)
	return fmt.Sprintf("%s/hls/%s/%d/%s/index.m3u8",
		strings.TrimRight(s.PublicBaseURL, "/"), camID, exp, tok), nil
}

// ProgressiveMP4URL builds the camera's stream URL plus a bearer token
// suitable for Google's CameraStream `cameraStreamAuthToken` field.
//
// Google passes the auth token in the Authorization header on the GET to
// `cameraStreamAccessUrl`, so we keep the URL itself clean of credentials
// and embed the expiry + HMAC inside the token.
//
// Token format: "<unix-exp>.<base64url-hmac(camID|exp)>".
func (s *Server) ProgressiveMP4URL(camID string) (url string, token string, err error) {
	exp := time.Now().Add(1 * time.Hour).Unix()
	sig := s.signToken(camID, exp)
	url = fmt.Sprintf("%s/mp4/%s/stream.mp4",
		strings.TrimRight(s.PublicBaseURL, "/"), camID)
	token = fmt.Sprintf("%d.%s", exp, sig)
	return url, token, nil
}

func (s *Server) signToken(camID string, exp int64) string {
	mac := hmac.New(sha256.New, s.StreamTokenSecret)
	_, _ = fmt.Fprintf(mac, "%s|%d", camID, exp)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) verifyToken(camID, expStr, tok string) error {
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return errors.New("bad exp")
	}
	if time.Now().Unix() > exp {
		return errors.New("token expired")
	}
	want := s.signToken(camID, exp)
	if !hmac.Equal([]byte(want), []byte(tok)) {
		return errors.New("bad signature")
	}
	return nil
}

// signalHandler accepts the SDP offer from Google and returns an SDP answer.
//
// Chromecast / Nest Hub clients issue a CORS preflight (OPTIONS) from
// https://www.gstatic.com before the actual POST, so we must answer it with
// the standard Access-Control-Allow-* headers. The URL itself is already
// HMAC-token-protected, so the CORS origin check adds no real security.
func (s *Server) signalHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	w.Header().Set("Access-Control-Max-Age", "600")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	camID := q.Get("cam")
	if err := s.verifyToken(camID, q.Get("exp"), q.Get("t")); err != nil {
		http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
		return
	}
	stream, ok := s.Registry.Get(camID)
	if !ok {
		http.Error(w, "unknown camera", http.StatusNotFound)
		return
	}

	var raw map[string]any
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		http.Error(w, "bad offer", http.StatusBadRequest)
		return
	}
	offerSDP, shape := extractOfferSDP(raw)
	if offerSDP == "" {
		log.Printf("signaling: empty offer (keys=%v)", mapKeys(raw))
		http.Error(w, "bad offer: no sdp", http.StatusBadRequest)
		return
	}
	log.Printf("signaling: camera %s offer shape=%s (sdp %d bytes)", camID, shape, len(offerSDP))

	_, answer, err := s.WebRTC.Negotiate(r.Context(), stream, wrtc.SignalingOffer{SDP: offerSDP, Type: "offer"})
	if err != nil {
		http.Error(w, "negotiate: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(buildAnswer(shape, answer.SDP))
}

// signalingShape is the request style used by the calling client. Each
// shape has a corresponding response style so the client can decode the
// answer it expects.
type signalingShape int

const (
	// shapeTypeSDP is the RFC-WebRTC shape: {"type":"offer","sdp":"..."}.
	// Used by Cast/Hub Max.
	shapeTypeSDP signalingShape = iota
	// shapeActionSDP is the Google Smart Home / Scrypted shape:
	// {"action":"offer","sdp":"..."}. Used by the Home phone app.
	shapeActionSDP
	// shapeOffer is the simplest shape from Google's published samples:
	// {"offer":"<sdp string>"}.
	shapeOffer
)

func (s signalingShape) String() string {
	switch s {
	case shapeActionSDP:
		return "action+sdp"
	case shapeOffer:
		return "offer"
	default:
		return "type+sdp"
	}
}

func extractOfferSDP(raw map[string]any) (string, signalingShape) {
	if v, ok := raw["action"].(string); ok && strings.EqualFold(v, "offer") {
		if sdp, ok := raw["sdp"].(string); ok && sdp != "" {
			return sdp, shapeActionSDP
		}
	}
	if v, ok := raw["offer"].(string); ok && v != "" {
		return v, shapeOffer
	}
	if sdp, ok := raw["sdp"].(string); ok && sdp != "" {
		return sdp, shapeTypeSDP
	}
	return "", shapeTypeSDP
}

func buildAnswer(shape signalingShape, sdp string) map[string]any {
	switch shape {
	case shapeActionSDP:
		return map[string]any{"action": "answer", "sdp": sdp}
	case shapeOffer:
		return map[string]any{"answer": sdp}
	default:
		return map[string]any{"type": "answer", "sdp": sdp}
	}
}

func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// hlsHandler validates the path-embedded HMAC token and forwards the
// request to the HLS server with the token segments stripped so gohlslib
// sees a clean "/<camID>/<rest>" path.
//
// Path layout: /hls/<camID>/<exp>/<sig>/<rest>
func (s *Server) hlsHandler(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/hls/")
	parts := strings.SplitN(trimmed, "/", 4)
	if len(parts) < 4 {
		http.Error(w, "bad hls path", http.StatusBadRequest)
		return
	}
	camID, expStr, sig, rest := parts[0], parts[1], parts[2], parts[3]
	if err := s.verifyToken(camID, expStr, sig); err != nil {
		http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
		return
	}
	r2 := *r
	u := *r.URL
	u.Path = "/" + camID + "/" + rest
	r2.URL = &u
	s.HLS.ServeHTTP(w, &r2)
}

// mp4Handler validates the bearer token Google passes in the Authorization
// header, then forwards the request to the progressive_mp4 server.
//
// URL layout: /mp4/<camID>/stream.mp4
// Authorization: Bearer <exp>.<hmac>
//
// We accept GET (Google's normal path) and also tolerate the rare HEAD
// the Test Suite uses for capability probes.
func (s *Server) mp4Handler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	trimmed := strings.TrimPrefix(r.URL.Path, "/mp4/")
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) < 2 {
		http.Error(w, "bad mp4 path", http.StatusBadRequest)
		return
	}
	camID := parts[0]

	hdr := r.Header.Get("Authorization")
	tok := strings.TrimPrefix(hdr, "Bearer ")
	if tok == hdr || tok == "" {
		http.Error(w, "missing bearer", http.StatusUnauthorized)
		return
	}
	dot := strings.IndexByte(tok, '.')
	if dot <= 0 {
		http.Error(w, "bad token", http.StatusUnauthorized)
		return
	}
	expStr, sig := tok[:dot], tok[dot+1:]
	if err := s.verifyToken(camID, expStr, sig); err != nil {
		http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
		return
	}

	r2 := *r
	u := *r.URL
	u.Path = "/" + camID + "/" + parts[1]
	r2.URL = &u
	s.MP4.ServeHTTP(w, &r2)
}
