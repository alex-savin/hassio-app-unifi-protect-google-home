package mp4

import (
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/rtsptest"
)

// TestMuxerStreamsFromRTSPS drives the real gortsplib client against an
// in-process RTSPS camera and checks the progressive stream is a valid
// fMP4: ftyp+moov init followed by moof+mdat fragments.
func TestMuxerStreamsFromRTSPS(t *testing.T) {
	cam := rtsptest.Start(t)
	m := NewMuxer("cam-1", cam.URL, false)
	t.Cleanup(m.Close)
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	var boxes []string
	for len(boxes) < 8 {
		var hdr [8]byte
		if _, err := io.ReadFull(resp.Body, hdr[:]); err != nil {
			t.Fatalf("read box header after %v: %v", boxes, err)
		}
		size := binary.BigEndian.Uint32(hdr[:4])
		if size < 8 {
			t.Fatalf("bad box size %d after %v", size, boxes)
		}
		boxes = append(boxes, string(hdr[4:]))
		if _, err := io.CopyN(io.Discard, resp.Body, int64(size-8)); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"ftyp", "moov", "moof", "mdat", "moof", "mdat", "moof", "mdat"}
	for i := range want {
		if boxes[i] != want[i] {
			t.Fatalf("boxes=%v want %v", boxes, want)
		}
	}
	if n := cam.Plays.Load(); n != 1 {
		t.Fatalf("camera PLAYs=%d want 1", n)
	}
}
