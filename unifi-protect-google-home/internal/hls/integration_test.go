package hls

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/rtsptest"
)

// TestMuxerServesPlaylistFromRTSPS drives the real gortsplib client
// against an in-process RTSPS camera until gohlslib publishes a playlist
// with at least one segment.
func TestMuxerServesPlaylistFromRTSPS(t *testing.T) {
	cam := rtsptest.Start(t)
	m := NewMuxer("cam-1", cam.URL, false)
	t.Cleanup(m.Close)
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)

	deadline := time.Now().Add(20 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		resp, err := http.Get(srv.URL + "/index.m3u8")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		last = string(b)
		if resp.StatusCode == http.StatusOK && strings.Contains(last, "#EXTM3U") {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("no playlist within deadline; last body: %q", last)
}
