package hls

import (
	"sync"
	"testing"
)

// mutableSource is an hls.Source whose answers the test can change.
type mutableSource struct {
	mu   sync.Mutex
	urls map[string]string
}

func (m *mutableSource) lookup(camID string) (string, bool, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.urls[camID]
	return u, false, ok
}

func (m *mutableSource) set(camID, url string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if url == "" {
		delete(m.urls, camID)
		return
	}
	m.urls[camID] = url
}

// TestMuxerForRechecksSource: a cached muxer must not outlive the camera
// being hidden/removed, and must be replaced when its RTSP URL changes.
func TestMuxerForRechecksSource(t *testing.T) {
	src := &mutableSource{urls: map[string]string{"cam-1": "rtsps://h/a"}}
	s := NewServer(src.lookup)

	first := s.muxerFor("cam-1")
	if first == nil {
		t.Fatal("known camera returned nil")
	}
	if s.muxerFor("cam-1") != first {
		t.Fatal("unchanged camera did not reuse its muxer")
	}

	src.set("cam-1", "rtsps://h/b")
	second := s.muxerFor("cam-1")
	if second == nil || second == first || second.RTSPURL != "rtsps://h/b" {
		t.Fatalf("URL change not picked up: %+v", second)
	}

	src.set("cam-1", "")
	if s.muxerFor("cam-1") != nil {
		t.Fatal("hidden camera still served")
	}
	if len(s.muxers) != 0 {
		t.Fatalf("hidden camera's muxer kept: %d", len(s.muxers))
	}
}

func TestPruneDropsIneligibleMuxers(t *testing.T) {
	src := &mutableSource{urls: map[string]string{
		"keep": "rtsps://h/k", "hide": "rtsps://h/h", "move": "rtsps://h/m",
	}}
	s := NewServer(src.lookup)
	for id := range src.urls {
		s.muxerFor(id)
	}
	src.set("hide", "")
	src.set("move", "rtsps://h/m2")
	s.Prune()
	if _, ok := s.muxers["keep"]; !ok || len(s.muxers) != 1 {
		t.Fatalf("after prune: %v", s.muxers)
	}
}
