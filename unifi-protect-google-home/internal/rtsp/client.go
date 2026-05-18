// Package rtsp implements a streams.Producer that pulls an RTSP/RTSPS feed
// using gortsplib and forwards H.264 RTP packets to pion WebRTC tracks.
//
// We do no transcoding. Audio is intentionally not forwarded — Protect cameras
// emit AAC, which WebRTC does not accept. Video-only is the supported mode.
package rtsp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"sync"

	gortsplib "github.com/bluenviron/gortsplib/v4"
	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/bluenviron/gortsplib/v4/pkg/description"
	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// Producer is a streams.Producer backed by an RTSP/RTSPS source.
type Producer struct {
	URL       string
	StreamID  string
	VerifyTLS bool

	mu     sync.Mutex
	client *gortsplib.Client
	track  *webrtc.TrackLocalStaticRTP
	closed bool
}

// NewProducer returns an idle Producer. URL must be rtsp:// or rtsps://.
func NewProducer(streamID, url string, verifyTLS bool) *Producer {
	return &Producer{URL: url, StreamID: streamID, VerifyTLS: verifyTLS}
}

// Start opens the RTSP session, locates an H.264 video media, creates a pion
// track, and begins forwarding RTP packets in a background goroutine.
func (p *Producer) Start(ctx context.Context) ([]*webrtc.TrackLocalStaticRTP, error) {
	u, err := base.ParseURL(p.URL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}

	transport := gortsplib.TransportTCP
	c := &gortsplib.Client{
		TLSConfig: &tls.Config{InsecureSkipVerify: !p.VerifyTLS}, //nolint:gosec
		Transport: &transport,
		Scheme:    u.Scheme,
		Host:      u.Host,
	}
	if err := c.Start2(); err != nil {
		return nil, fmt.Errorf("rtsp start: %w", err)
	}

	desc, _, err := c.Describe(u)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("describe: %w", err)
	}

	var videoMedia *description.Media
	var videoFormat *format.H264
	for _, medi := range desc.Medias {
		for _, forma := range medi.Formats {
			if h264, ok := forma.(*format.H264); ok {
				videoMedia = medi
				videoFormat = h264
				break
			}
		}
		if videoFormat != nil {
			break
		}
	}
	if videoFormat == nil {
		c.Close()
		return nil, errors.New("no H.264 video track in RTSP stream")
	}

	if err := c.SetupAll(desc.BaseURL, []*description.Media{videoMedia}); err != nil {
		c.Close()
		return nil, fmt.Errorf("setup: %w", err)
	}

	profileLevelID := profileLevelIDFromSPS(videoFormat.SPS)
	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=" + profileLevelID,
		},
		"video", p.StreamID,
	)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("new track: %w", err)
	}

	c.OnPacketRTP(videoMedia, videoFormat, func(pkt *rtp.Packet) {
		if err := track.WriteRTP(pkt); err != nil {
			log.Printf("rtsp[%s] write rtp: %v", p.StreamID, err)
		}
	})

	if _, err := c.Play(nil); err != nil {
		c.Close()
		return nil, fmt.Errorf("play: %w", err)
	}

	p.mu.Lock()
	p.client = c
	p.track = track
	p.mu.Unlock()

	go func() {
		errCh := make(chan error, 1)
		go func() { errCh <- c.Wait() }()
		select {
		case <-ctx.Done():
			c.Close()
		case err := <-errCh:
			if err != nil {
				log.Printf("rtsp[%s] session ended: %v", p.StreamID, err)
			}
		}
	}()

	return []*webrtc.TrackLocalStaticRTP{track}, nil
}

// Stop terminates the RTSP session.
func (p *Producer) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	if p.client != nil {
		p.client.Close()
		p.client = nil
	}
	p.track = nil
	return nil
}
