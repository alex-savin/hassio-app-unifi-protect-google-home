package rtsp

import (
	"context"
	"testing"
	"time"

	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/rtsptest"
)

// TestProducerStartsAgainstRTSPS checks DESCRIBE/SETUP/PLAY over RTSPS
// with the real gortsplib client and that Stop releases the session.
func TestProducerStartsAgainstRTSPS(t *testing.T) {
	cam := rtsptest.Start(t)
	p := NewProducer("cam-1", cam.URL, false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tracks, err := p.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 1 {
		t.Fatalf("tracks=%d want 1", len(tracks))
	}
	if n := cam.Plays.Load(); n != 1 {
		t.Fatalf("camera PLAYs=%d want 1", n)
	}
	// Let a few frames flow through the decode callback.
	time.Sleep(300 * time.Millisecond)
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
}
