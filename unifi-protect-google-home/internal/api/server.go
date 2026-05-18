// the optional admin endpoints.
package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/discovery"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/ghome"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/oauth"
	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/streams"
	wrtc "github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/webrtc"
)

// DiscoverFunc runs a UBNT UDP discovery scan and returns the result.
// Indirected so tests don't actually open sockets.
type DiscoverFunc func(ctx context.Context) ([]discovery.Device, error)

// Server hosts every public HTTP endpoint.
type Server struct {
	PublicBaseURL     string
	StreamTokenSecret []byte

	OAuth    *oauth.Server
	Fulfill  *ghome.Handler
	Registry *streams.Registry
	WebRTC   *wrtc.Factory

	// Discover, when non-nil, enables GET /admin/discover.
	Discover DiscoverFunc
}

// Routes returns an http.Handler with all endpoints mounted.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/authorize", s.OAuth.Authorize)
	mux.HandleFunc("/oauth/token", s.OAuth.Token)
	mux.Handle("/smarthome", s.authMiddleware(s.Fulfill))
	mux.HandleFunc("/webrtc/signal", s.signalHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	if s.Discover != nil {
		mux.HandleFunc("/admin/discover", s.discoverHandler)
	}
	return mux
}

// discoverHandler runs a UBNT discovery scan and returns the result as JSON.
// Exposes only LAN-visible metadata (IP/MAC/hostname/model) so it is not
// authenticated — anyone with network access to the bridge already has the
// same view via a raw UDP broadcast.
func (s *Server) discoverHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	devices, err := s.Discover(ctx)
	if err != nil {
		http.Error(w, "discover: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if devices == nil {
		devices = []discovery.Device{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"devices": devices})
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

	var offer wrtc.SignalingOffer
	if err := json.NewDecoder(r.Body).Decode(&offer); err != nil {
		http.Error(w, "bad offer", http.StatusBadRequest)
		return
	}

	_, answer, err := s.WebRTC.Negotiate(r.Context(), stream, offer)
	if err != nil {
		http.Error(w, "negotiate: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(answer)
}
