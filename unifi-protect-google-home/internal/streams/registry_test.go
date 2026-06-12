package streams

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"

	"github.com/pion/webrtc/v4"
)

// fakeProducer records Start/Stop calls and hands out a fixed track list.
type fakeProducer struct {
	mu         sync.Mutex
	startCalls int
	stopCalls  int
	startCtx   context.Context // ctx passed to the most recent Start
	tracks     []webrtc.TrackLocal
	startErr   error
}

func (p *fakeProducer) Start(ctx context.Context) ([]webrtc.TrackLocal, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.startCalls++
	p.startCtx = ctx
	if p.startErr != nil {
		return nil, p.startErr
	}
	return p.tracks, nil
}

func (p *fakeProducer) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopCalls++
	return nil
}

func (p *fakeProducer) counts() (starts, stops int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.startCalls, p.stopCalls
}

func (p *fakeProducer) lastCtx() context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.startCtx
}

func newFakeTrack(t *testing.T) webrtc.TrackLocal {
	t.Helper()
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "test")
	if err != nil {
		t.Fatalf("new track: %v", err)
	}
	return track
}

func newFakeProducer(t *testing.T) *fakeProducer {
	t.Helper()
	return &fakeProducer{tracks: []webrtc.TrackLocal{newFakeTrack(t)}}
}

func ctxDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

func TestStreamAcquireRefCounting(t *testing.T) {
	p := newFakeProducer(t)
	s := NewStream("cam1", p)

	tracks1, release1, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if len(tracks1) != 1 || tracks1[0] != p.tracks[0] {
		t.Fatalf("first acquire tracks = %v, want producer's track", tracks1)
	}
	if starts, _ := p.counts(); starts != 1 {
		t.Fatalf("starts = %d after first acquire, want 1", starts)
	}

	// Second acquire while held must reuse the running producer.
	tracks2, release2, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if tracks2[0] != tracks1[0] {
		t.Fatal("second acquire returned different tracks")
	}
	if starts, _ := p.counts(); starts != 1 {
		t.Fatalf("starts = %d after second acquire, want 1 (no restart while held)", starts)
	}

	// Releasing one of two refs must not stop the producer.
	release1()
	if _, stops := p.counts(); stops != 0 {
		t.Fatalf("stops = %d after releasing first ref, want 0", stops)
	}
	if ctxDone(p.lastCtx()) {
		t.Fatal("start ctx cancelled while a ref is still held")
	}

	// Releasing the last ref stops the producer and cancels its ctx.
	release2()
	if _, stops := p.counts(); stops != 1 {
		t.Fatalf("stops = %d after releasing last ref, want 1", stops)
	}
	if !ctxDone(p.lastCtx()) {
		t.Fatal("start ctx not cancelled after last release")
	}
}

func TestStreamRestartAfterFullRelease(t *testing.T) {
	p := newFakeProducer(t)
	s := NewStream("cam1", p)

	_, release, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	release()

	tracks, release, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	defer release()
	if len(tracks) != 1 {
		t.Fatalf("got %d tracks on re-acquire, want 1", len(tracks))
	}
	starts, stops := p.counts()
	if starts != 2 || stops != 1 {
		t.Fatalf("starts=%d stops=%d after re-acquire, want starts=2 stops=1", starts, stops)
	}
}

func TestStreamStartError(t *testing.T) {
	p := newFakeProducer(t)
	p.startErr = errors.New("rtsp connect failed")
	s := NewStream("cam1", p)

	if _, _, err := s.Acquire(context.Background()); err == nil {
		t.Fatal("acquire succeeded, want producer start error")
	}
	starts, stops := p.counts()
	if starts != 1 || stops != 0 {
		t.Fatalf("starts=%d stops=%d after failed start, want starts=1 stops=0", starts, stops)
	}
	if !ctxDone(p.lastCtx()) {
		t.Fatal("start ctx not cancelled after failed start")
	}

	// The failure must not leave the stream marked started: a later acquire
	// retries the producer.
	p.mu.Lock()
	p.startErr = nil
	p.mu.Unlock()
	_, release, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire after recovery: %v", err)
	}
	defer release()
	if starts, _ := p.counts(); starts != 2 {
		t.Fatalf("starts = %d after recovery acquire, want 2", starts)
	}
}

func TestStreamZeroTracks(t *testing.T) {
	p := &fakeProducer{} // no tracks configured
	s := NewStream("cam1", p)

	if _, _, err := s.Acquire(context.Background()); err == nil {
		t.Fatal("acquire succeeded, want zero-tracks error")
	}
	starts, stops := p.counts()
	if starts != 1 || stops != 1 {
		t.Fatalf("starts=%d stops=%d, want starts=1 stops=1 (producer stopped on zero tracks)", starts, stops)
	}
	if !ctxDone(p.lastCtx()) {
		t.Fatal("start ctx not cancelled after zero-tracks failure")
	}
}

func TestRegistryBasics(t *testing.T) {
	r := NewRegistry()
	if names := r.Names(); len(names) != 0 {
		t.Fatalf("new registry names = %v, want empty", names)
	}
	if _, ok := r.Get("front"); ok {
		t.Fatal("Get on empty registry returned ok")
	}

	front := NewStream("front", &fakeProducer{})
	back := NewStream("back", &fakeProducer{})
	r.Put(front)
	r.Put(back)

	got, ok := r.Get("front")
	if !ok || got != front {
		t.Fatalf("Get(front) = %v, %v; want the stored stream", got, ok)
	}
	names := r.Names()
	sort.Strings(names)
	if len(names) != 2 || names[0] != "back" || names[1] != "front" {
		t.Fatalf("names = %v, want [back front]", names)
	}

	// Put replaces an existing entry under the same name.
	front2 := NewStream("front", &fakeProducer{})
	r.Put(front2)
	if got, _ := r.Get("front"); got != front2 {
		t.Fatal("Put did not replace existing stream")
	}

	r.Delete("front")
	if _, ok := r.Get("front"); ok {
		t.Fatal("Get(front) ok after Delete")
	}
	if names := r.Names(); len(names) != 1 || names[0] != "back" {
		t.Fatalf("names after delete = %v, want [back]", names)
	}
	// Deleting a missing name is a no-op.
	r.Delete("front")
}

func TestStreamConcurrentAcquireRelease(t *testing.T) {
	p := newFakeProducer(t)
	s := NewStream("cam1", p)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tracks, release, err := s.Acquire(context.Background())
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			if len(tracks) != 1 {
				t.Errorf("got %d tracks, want 1", len(tracks))
			}
			release()
		}()
	}
	wg.Wait()

	// All refs released: every Start must have a matching Stop and the
	// stream must be restartable.
	starts, stops := p.counts()
	if starts == 0 || starts != stops {
		t.Fatalf("starts=%d stops=%d after all releases, want equal and > 0", starts, stops)
	}
	_, release, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire after concurrent churn: %v", err)
	}
	release()
}
