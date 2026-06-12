// Package rtsp implements a streams.Producer that pulls an RTSP/RTSPS feed
// using gortsplib and forwards H.264 access units to pion WebRTC tracks
// via TrackLocalStaticSample.
//
// We do no transcoding. Audio is intentionally not forwarded — Protect cameras
// emit AAC, which WebRTC does not accept. Video-only is the supported mode.
//
// Why samples instead of raw RTP forwarding: pion's outbound RTP path has a
// hardcoded 1200-byte MTU (see pion/webrtc/v4 constants.go outboundMTU). RTSP
// sources typically emit 1450-byte RTP packets sized for Ethernet UDP, which
// fail SRTP/ICE buffer checks with io.ErrShortBuffer. By depacketizing to
// H.264 access units and writing them through TrackLocalStaticSample, pion's
// internal H.264 payloader fragments NALUs into FU-A packets that respect
// the outbound MTU.
package rtsp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	gortsplib "github.com/bluenviron/gortsplib/v4"
	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/bluenviron/gortsplib/v4/pkg/description"
	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph264"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// annexBStartCode is the 4-byte Annex-B NALU start prefix.
var annexBStartCode = []byte{0x00, 0x00, 0x00, 0x01}

// h264NALUTypeIDR is the NAL unit type for an instantaneous decoder refresh
// (key frame). When we see one, we prepend SPS/PPS so downstream decoders
// (Chromecast / Google Home displays) have parameter sets to initialize.
const h264NALUTypeIDR = 5

// Producer is a streams.Producer backed by an RTSP/RTSPS source. Once
// started it keeps the upstream alive — reconnecting with backoff when the
// camera drops — until Stop is called or the start context is cancelled.
// The pion track survives reconnects, so attached PeerConnections resume
// instead of freezing on the last frame.
type Producer struct {
	URL       string
	StreamID  string
	VerifyTLS bool

	mu     sync.Mutex
	client *gortsplib.Client
	track  *webrtc.TrackLocalStaticSample
	closed bool
}

// NewProducer returns an idle Producer. URL must be rtsp:// or rtsps://.
func NewProducer(streamID, url string, verifyTLS bool) *Producer {
	return &Producer{URL: url, StreamID: streamID, VerifyTLS: verifyTLS}
}

// Start opens the RTSP session, locates an H.264 video media, creates a pion
// sample track, and begins forwarding access units in a background goroutine.
func (p *Producer) Start(ctx context.Context) ([]webrtc.TrackLocal, error) {
	c, videoFormat, err := p.connect()
	if err != nil {
		return nil, err
	}

	profileLevelID := profileLevelIDFromSPS(videoFormat.SPS)
	track, err := webrtc.NewTrackLocalStaticSample(
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

	p.mu.Lock()
	p.client = c
	p.track = track
	p.closed = false // producers are restartable (Acquire → release → Acquire)
	p.mu.Unlock()

	go p.run(ctx, c)

	return []webrtc.TrackLocal{track}, nil
}

// connect dials the source, finds the H.264 media, and starts playback.
// Decoded access units are written to whatever track is current at the
// moment of arrival, so sessions created by reconnects feed the same track
// the consumers already hold.
func (p *Producer) connect() (*gortsplib.Client, *format.H264, error) {
	u, err := base.ParseURL(p.URL)
	if err != nil {
		return nil, nil, fmt.Errorf("parse url: %w", err)
	}

	transport := gortsplib.TransportTCP
	c := &gortsplib.Client{
		TLSConfig: &tls.Config{InsecureSkipVerify: !p.VerifyTLS}, //nolint:gosec
		Transport: &transport,
		Scheme:    u.Scheme,
		Host:      u.Host,
	}
	if err := c.Start2(); err != nil {
		return nil, nil, fmt.Errorf("rtsp start: %w", err)
	}

	desc, _, err := c.Describe(u)
	if err != nil {
		c.Close()
		return nil, nil, fmt.Errorf("describe: %w", err)
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
		return nil, nil, errors.New("no H.264 video track in RTSP stream")
	}

	if err := c.SetupAll(desc.BaseURL, []*description.Media{videoMedia}); err != nil {
		c.Close()
		return nil, nil, fmt.Errorf("setup: %w", err)
	}

	rtpDec, err := videoFormat.CreateDecoder()
	if err != nil {
		c.Close()
		return nil, nil, fmt.Errorf("create h264 decoder: %w", err)
	}

	sps := videoFormat.SPS
	pps := videoFormat.PPS

	var (
		lastTS   uint32
		haveTS   bool
		fallback = 33 * time.Millisecond // ~30fps default for the first sample
	)

	c.OnPacketRTP(videoMedia, videoFormat, func(pkt *rtp.Packet) {
		au, err := rtpDec.Decode(pkt)
		if err != nil {
			if errors.Is(err, rtph264.ErrMorePacketsNeeded) ||
				errors.Is(err, rtph264.ErrNonStartingPacketAndNoPrevious) {
				return
			}
			log.Printf("rtsp[%s] decode: %v", p.StreamID, err)
			return
		}

		// Inject SPS/PPS before any IDR so downstream decoders can init.
		au = injectParamSets(au, sps, pps)

		// Compute sample duration from RTP timestamp delta (90kHz clock).
		var dur time.Duration
		if haveTS {
			delta := int32(pkt.Timestamp - lastTS) //nolint:gosec // wrap is intentional
			if delta <= 0 {
				dur = fallback
			} else {
				dur = time.Duration(delta) * time.Second / 90000
			}
		} else {
			dur = fallback
		}
		lastTS = pkt.Timestamp
		haveTS = true

		track := p.currentTrack()
		if track == nil {
			return // producer stopped, or first session racing Start
		}
		data := encodeAnnexB(au)
		if err := track.WriteSample(media.Sample{Data: data, Duration: dur}); err != nil {
			log.Printf("rtsp[%s] write sample: %v", p.StreamID, err)
		}
	})

	if _, err := c.Play(nil); err != nil {
		c.Close()
		return nil, nil, fmt.Errorf("play: %w", err)
	}

	return c, videoFormat, nil
}

// run watches the current session and reconnects when it dies while
// consumers are still attached. It exits when ctx is cancelled (last
// consumer released) or Stop is called.
func (p *Producer) run(ctx context.Context, c *gortsplib.Client) {
	const maxBackoff = 30 * time.Second
	backoff := time.Second
	for {
		connectedAt := time.Now()
		errCh := make(chan error, 1)
		go func() { errCh <- c.Wait() }()
		select {
		case <-ctx.Done():
			c.Close()
			return
		case err := <-errCh:
			if err != nil {
				log.Printf("rtsp[%s] session ended: %v", p.StreamID, err)
			}
		}
		if time.Since(connectedAt) > 30*time.Second {
			backoff = time.Second
		}

		for {
			if ctx.Err() != nil || p.isClosed() {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < maxBackoff {
				backoff *= 2
			}
			nc, _, err := p.connect()
			if err != nil {
				log.Printf("rtsp[%s] reconnect: %v", p.StreamID, err)
				continue
			}
			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				nc.Close()
				return
			}
			p.client = nc
			p.mu.Unlock()
			log.Printf("rtsp[%s] reconnected", p.StreamID)
			c = nc
			break
		}
	}
}

func (p *Producer) currentTrack() *webrtc.TrackLocalStaticSample {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.track
}

func (p *Producer) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// Stop terminates the RTSP session and the reconnect loop.
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

// encodeAnnexB concatenates NAL units with 4-byte Annex-B start codes.
func encodeAnnexB(au [][]byte) []byte {
	size := 0
	for _, nalu := range au {
		size += 4 + len(nalu)
	}
	out := make([]byte, 0, size)
	for _, nalu := range au {
		out = append(out, annexBStartCode...)
		out = append(out, nalu...)
	}
	return out
}

// injectParamSets prepends SPS/PPS to the access unit if it contains an IDR
// and the AU doesn't already carry parameter sets. Returns the AU unchanged
// if no IDR is present or SPS/PPS are unavailable.
func injectParamSets(au [][]byte, sps, pps []byte) [][]byte {
	if len(sps) == 0 || len(pps) == 0 {
		return au
	}
	hasIDR, hasSPS, hasPPS := false, false, false
	for _, nalu := range au {
		if len(nalu) == 0 {
			continue
		}
		switch nalu[0] & 0x1F {
		case h264NALUTypeIDR:
			hasIDR = true
		case 7: // SPS
			hasSPS = true
		case 8: // PPS
			hasPPS = true
		}
	}
	if !hasIDR || (hasSPS && hasPPS) {
		return au
	}
	prefix := make([][]byte, 0, 2+len(au))
	if !hasSPS {
		prefix = append(prefix, sps)
	}
	if !hasPPS {
		prefix = append(prefix, pps)
	}
	return append(prefix, au...)
}
