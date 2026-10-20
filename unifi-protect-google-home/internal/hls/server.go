package hls

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// Server keeps a map of per-camera Muxers and dispatches incoming HTTP
// requests by URL prefix. The caller's HTTP router is expected to mount
// Server.ServeHTTP under some path (e.g. "/hls/") and pass requests through
// after stripping that prefix; the Server then peels the next path segment
// as the camera ID and routes the remainder to that camera's Muxer.
type Server struct {
	// LookupRTSP returns the upstream RTSP URL for a camera ID. Required.
	LookupRTSP Source

	mu      sync.Mutex
	muxers  map[string]*Muxer
	closing atomic.Bool
}

// NewServer returns an empty Server.
func NewServer(lookup Source) *Server {
	return &Server{LookupRTSP: lookup, muxers: map[string]*Muxer{}}
}

// ServeHTTP routes a request shaped like /<camID>/<rest...> to the muxer
// for camID, lazily creating it on first request.
//
// CORS preflight is answered with permissive headers because the
// per-request URL is HMAC-signed elsewhere — the browser-side origin check
// adds no real security.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.closing.Load() {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/")
	camID, rest, _ := strings.Cut(path, "/")
	if camID == "" {
		http.Error(w, "missing camera id", http.StatusBadRequest)
		return
	}
	mux := s.muxerFor(camID)
	if mux == nil {
		http.Error(w, "unknown camera", http.StatusNotFound)
		return
	}
	// gohlslib.Muxer.Handle inspects r.URL.Path, so rewrite it relative
	// to the camera root.
	r2 := *r
	u := *r.URL
	u.Path = "/" + rest
	r2.URL = &u
	mux.ServeHTTP(w, &r2)
}

// muxerFor resolves camID on every request rather than trusting the cached
// muxer: a camera that was hidden or removed must stop serving immediately,
// and one whose RTSP URL changed (stream re-enabled in Protect) must not
// keep dialing the dead address.
func (s *Server) muxerFor(camID string) *Muxer {
	s.mu.Lock()
	defer s.mu.Unlock()
	url, verifyTLS, ok := s.LookupRTSP(camID)
	m, exists := s.muxers[camID]
	if exists && (!ok || m.RTSPURL != url || m.VerifyTLS != verifyTLS) {
		m.Close()
		delete(s.muxers, camID)
		exists = false
	}
	if !ok {
		return nil
	}
	if exists {
		return m
	}
	m = NewMuxer(camID, url, verifyTLS)
	s.muxers[camID] = m
	return m
}

// Prune closes the muxers of cameras that are no longer exposed, were
// removed, or changed upstream URL. Call it whenever the camera set or the
// exposure allow-list changes so in-flight streams are cut off without
// waiting for the next request.
func (s *Server) Prune() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, m := range s.muxers {
		url, verifyTLS, ok := s.LookupRTSP(id)
		if !ok || m.RTSPURL != url || m.VerifyTLS != verifyTLS {
			m.Close()
			delete(s.muxers, id)
		}
	}
}

// Shutdown closes all muxers. Call once on bridge exit.
func (s *Server) Shutdown() {
	s.closing.Store(true)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.muxers {
		m.Close()
	}
	s.muxers = map[string]*Muxer{}
}
