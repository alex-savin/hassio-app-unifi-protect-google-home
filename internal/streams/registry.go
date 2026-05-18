// Package streams maintains a reference-counted registry of live media streams.
//
// Each Stream has a Producer that, when started, opens an upstream source
// (e.g. RTSP) and writes RTP packets into one or more pion TrackLocalStaticRTP
// tracks. Those tracks can be added to many WebRTC PeerConnections at once;
// pion handles per-sender fan-out. When the last consumer detaches, the
// Producer is stopped to release the upstream.
package streams

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/pion/webrtc/v4"
)

// Producer opens an upstream media source and writes RTP packets into the
// Tracks it returns from Start.
//
// Lifecycle: Start blocks only long enough to negotiate the source and
// populate Tracks; it must spawn its own goroutine for the long-running read
// loop, which should exit when ctx is cancelled or Stop is called.
type Producer interface {
	Start(ctx context.Context) ([]*webrtc.TrackLocalStaticRTP, error)
	Stop() error
}

// Stream couples a name with a Producer and tracks consumers.
type Stream struct {
	Name     string
	Producer Producer

	mu      sync.Mutex
	tracks  []*webrtc.TrackLocalStaticRTP
	refs    int
	started bool
	cancel  context.CancelFunc
}

// NewStream returns an idle Stream wrapping the given Producer.
func NewStream(name string, p Producer) *Stream {
	return &Stream{Name: name, Producer: p}
}

// Acquire starts the producer on first ref. Returns the active tracks and
// a release function that the caller MUST call exactly once when done.
func (s *Stream) Acquire(ctx context.Context) ([]*webrtc.TrackLocalStaticRTP, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		runCtx, cancel := context.WithCancel(context.Background())
		tracks, err := s.Producer.Start(runCtx)
		if err != nil {
			cancel()
			return nil, nil, fmt.Errorf("producer start: %w", err)
		}
		if len(tracks) == 0 {
			cancel()
			_ = s.Producer.Stop()
			return nil, nil, errors.New("producer yielded no tracks")
		}
		s.tracks = tracks
		s.cancel = cancel
		s.started = true
	}
	s.refs++
	tracks := s.tracks
	return tracks, s.release, nil
}

func (s *Stream) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refs--
	if s.refs > 0 || !s.started {
		return
	}
	s.cancel()
	_ = s.Producer.Stop()
	s.started = false
	s.tracks = nil
	s.cancel = nil
}

// Registry maps stream names → *Stream and is safe for concurrent use.
type Registry struct {
	mu      sync.RWMutex
	streams map[string]*Stream
}

func NewRegistry() *Registry { return &Registry{streams: map[string]*Stream{}} }

// Put inserts or replaces a stream. Replacing a started stream is the
// caller's responsibility (release in-flight refs first).
func (r *Registry) Put(s *Stream) {
	r.mu.Lock()
	r.streams[s.Name] = s
	r.mu.Unlock()
}

// Delete removes a stream entry.
func (r *Registry) Delete(name string) {
	r.mu.Lock()
	delete(r.streams, name)
	r.mu.Unlock()
}

func (r *Registry) Get(name string) (*Stream, bool) {
	r.mu.RLock()
	s, ok := r.streams[name]
	r.mu.RUnlock()
	return s, ok
}

// Names returns a snapshot of registered stream names.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.streams))
	for n := range r.streams {
		names = append(names, n)
	}
	return names
}
