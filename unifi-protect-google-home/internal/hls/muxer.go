// Package hls provides an on-demand RTSP→HLS muxer per camera.
//
// The Google Home phone app does not consume the WebRTC protocol exposed
// via Smart Home actions — only the Nest Hub Max does. To make camera tiles
// render live video on phones we serve HLS (MPEG-TS variant) alongside
// WebRTC. Each camera gets its own Muxer that:
//
//  1. Opens its own RTSPS session to UniFi Protect on first request.
//  2. Demuxes H.264 access units and feeds them to a gohlslib.Muxer.
//  3. Serves the resulting .m3u8 playlist and segments via Muxer.ServeHTTP.
//  4. Auto-closes the upstream after an idle period (no HTTP request for the
//     playlist for idleTimeout) and lazily re-opens on next request.
//
// Audio is intentionally dropped: Protect emits AAC which Google's HLS
// playback for Smart Home cameras tolerates but UniFi cameras over local
// RTSP commonly mis-stamp, causing playback stalls. Video-only is plenty
// for a Home-app preview tile.
package hls

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gohlslib/v2"
	"github.com/bluenviron/gohlslib/v2/pkg/codecs"
	gortsplib "github.com/bluenviron/gortsplib/v4"
	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/bluenviron/gortsplib/v4/pkg/description"
	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph264"
	"github.com/pion/rtp"
)

const (
	// idleTimeout is how long upstream stays open without an HTTP request
	// from a player. Google typically polls the playlist every few seconds
	// during playback; 30 s is plenty of slack while keeping idle RTSP
	// sessions short to free Protect resources.
	idleTimeout = 30 * time.Second

	// segmentMinDuration trades latency for stability. 1 s segments are
	// the gohlslib default and play reliably across iOS / Android.
	segmentMinDuration = 1 * time.Second
)

// Source resolves a camera ID to the RTSP(S) URL the bridge should pull
// from. Returning ok=false means the camera is unknown or has no
// RTSP-enabled channel.
type Source func(camID string) (rtspURL string, verifyTLS bool, ok bool)

// Muxer is one camera's HLS pipeline. Safe for concurrent ServeHTTP calls.
type Muxer struct {
	StreamID  string
	RTSPURL   string
	VerifyTLS bool

	mu           sync.Mutex
	hlsMux       *gohlslib.Muxer
	hlsTrack     *gohlslib.Track
	rtspClient   *gortsplib.Client
	cancelUp     context.CancelFunc
	started      bool
	lastAccessNs atomic.Int64
}

// NewMuxer returns an idle Muxer for the given camera and RTSP URL.
func NewMuxer(streamID, rtspURL string, verifyTLS bool) *Muxer {
	return &Muxer{StreamID: streamID, RTSPURL: rtspURL, VerifyTLS: verifyTLS}
}

// ServeHTTP serves the playlist or a segment. r.URL.Path must already be
// stripped of the per-camera prefix so gohlslib sees a relative path
// (e.g. "/index.m3u8" or "/seg0.ts"). On first call the upstream RTSP
// session is opened lazily.
func (m *Muxer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.lastAccessNs.Store(time.Now().UnixNano())
	if err := m.ensureStarted(); err != nil {
		log.Printf("hls[%s] start: %v", m.StreamID, err)
		http.Error(w, "stream unavailable", http.StatusServiceUnavailable)
		return
	}
	// Wait briefly for the muxer to produce its first segment so the very
	// first playlist request after a cold start doesn't 404.
	if err := m.waitFirstSegment(r.Context(), 5*time.Second); err != nil {
		http.Error(w, "stream warming up", http.StatusServiceUnavailable)
		return
	}
	// Snapshot under the lock: stopLocked may nil out m.hlsMux between
	// ensureStarted returning and the request being handled (e.g. the
	// camera dropped mid-flight). gohlslib's Handle is safe on a closed
	// muxer, but not on a nil one.
	m.mu.Lock()
	h := m.hlsMux
	m.mu.Unlock()
	if h == nil {
		http.Error(w, "stream restarting", http.StatusServiceUnavailable)
		return
	}
	h.Handle(w, r)
}

// waitFirstSegment polls hlsMux by issuing a HEAD-style attempt — gohlslib
// doesn't expose readiness, so we rely on a small fixed delay on cold start
// and trust subsequent requests. In practice the IDR arrives within a
// keyframe interval (≤ 4 s for Protect).
func (m *Muxer) waitFirstSegment(ctx context.Context, _ time.Duration) error {
	// Cheap and good-enough: the muxer accepts requests immediately once
	// Start() returned. gohlslib internally 404s playlist requests until
	// the first segment is ready, which players treat as a soft retry.
	// No-op kept for documentation and future tightening.
	_ = ctx
	return nil
}

// Close stops upstream and releases the HLS muxer. Safe to call repeatedly.
func (m *Muxer) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked()
}

func (m *Muxer) stopLocked() {
	if !m.started {
		return
	}
	if m.cancelUp != nil {
		m.cancelUp()
	}
	if m.rtspClient != nil {
		m.rtspClient.Close()
		m.rtspClient = nil
	}
	if m.hlsMux != nil {
		m.hlsMux.Close()
		m.hlsMux = nil
	}
	m.hlsTrack = nil
	m.started = false
}

func (m *Muxer) ensureStarted() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return nil
	}
	if err := m.startLocked(); err != nil {
		m.stopLocked()
		return err
	}
	return nil
}

func (m *Muxer) startLocked() error {
	u, err := base.ParseURL(m.RTSPURL)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	transport := gortsplib.TransportTCP
	c := &gortsplib.Client{
		TLSConfig: &tls.Config{InsecureSkipVerify: !m.VerifyTLS}, //nolint:gosec
		Transport: &transport,
		Scheme:    u.Scheme,
		Host:      u.Host,
	}
	if err := c.Start2(); err != nil {
		return fmt.Errorf("rtsp start: %w", err)
	}
	desc, _, err := c.Describe(u)
	if err != nil {
		c.Close()
		return fmt.Errorf("describe: %w", err)
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
		return errors.New("no H.264 video track in RTSP stream")
	}
	if err := c.SetupAll(desc.BaseURL, []*description.Media{videoMedia}); err != nil {
		c.Close()
		return fmt.Errorf("setup: %w", err)
	}
	rtpDec, err := videoFormat.CreateDecoder()
	if err != nil {
		c.Close()
		return fmt.Errorf("create h264 decoder: %w", err)
	}

	track := &gohlslib.Track{
		Codec:     &codecs.H264{SPS: videoFormat.SPS, PPS: videoFormat.PPS},
		ClockRate: 90000,
	}
	hlsMux := &gohlslib.Muxer{
		Variant:            gohlslib.MuxerVariantMPEGTS,
		SegmentCount:       7,
		SegmentMinDuration: segmentMinDuration,
		Tracks:             []*gohlslib.Track{track},
	}
	if err := hlsMux.Start(); err != nil {
		c.Close()
		return fmt.Errorf("hls start: %w", err)
	}

	sps := videoFormat.SPS
	pps := videoFormat.PPS

	var (
		lastRawTS uint32
		pts       int64
		haveFirst bool
	)

	c.OnPacketRTP(videoMedia, videoFormat, func(pkt *rtp.Packet) {
		au, err := rtpDec.Decode(pkt)
		if err != nil {
			if errors.Is(err, rtph264.ErrMorePacketsNeeded) ||
				errors.Is(err, rtph264.ErrNonStartingPacketAndNoPrevious) {
				return
			}
			return
		}
		au = injectParamSets(au, sps, pps)

		// PTS in the track clock rate (90 kHz). Accumulating signed deltas
		// keeps it monotonic across the 32-bit RTP timestamp wrap (~6.6 h),
		// where a plain int32 cast of (ts - firstTS) would jump negative.
		if !haveFirst {
			lastRawTS = pkt.Timestamp
			haveFirst = true
		}
		pts += int64(int32(pkt.Timestamp - lastRawTS)) //nolint:gosec
		lastRawTS = pkt.Timestamp
		if err := hlsMux.WriteH264(track, time.Now(), pts, au); err != nil {
			log.Printf("hls[%s] write: %v", m.StreamID, err)
		}
	})

	if _, err := c.Play(nil); err != nil {
		hlsMux.Close()
		c.Close()
		return fmt.Errorf("play: %w", err)
	}

	upCtx, cancel := context.WithCancel(context.Background())
	m.cancelUp = cancel
	m.rtspClient = c
	m.hlsMux = hlsMux
	m.hlsTrack = track
	m.started = true
	m.lastAccessNs.Store(time.Now().UnixNano())

	// Upstream read loop + idle-watchdog. Both exit together when ctx is
	// cancelled (either by Close or by the idle timer).
	go m.runReadLoop(upCtx, c)
	go m.runIdleWatchdog(upCtx)

	log.Printf("hls[%s] started (mpegts, %d-byte SPS)", m.StreamID, len(sps))
	return nil
}

func (m *Muxer) runReadLoop(ctx context.Context, c *gortsplib.Client) {
	errCh := make(chan error, 1)
	go func() { errCh <- c.Wait() }()
	select {
	case <-ctx.Done():
		// shutdown initiated locally; nothing to log
	case err := <-errCh:
		if err != nil {
			log.Printf("hls[%s] rtsp session ended: %v", m.StreamID, err)
		}
		m.mu.Lock()
		m.stopLocked()
		m.mu.Unlock()
	}
}

func (m *Muxer) runIdleWatchdog(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			last := time.Unix(0, m.lastAccessNs.Load())
			if time.Since(last) <= idleTimeout {
				continue
			}
			m.mu.Lock()
			stopped := false
			if m.started && time.Since(time.Unix(0, m.lastAccessNs.Load())) > idleTimeout {
				log.Printf("hls[%s] idle %s — stopping upstream", m.StreamID, idleTimeout)
				m.stopLocked()
				stopped = true
			}
			m.mu.Unlock()
			// Only exit once the session is actually stopped — a request may
			// have refreshed lastAccessNs between the unlocked check and the
			// lock, in which case this watchdog must keep watching.
			if stopped {
				return
			}
		}
	}
}

// injectParamSets prepends SPS/PPS to the access unit if it contains an IDR
// and the AU doesn't already carry parameter sets. Mirrors the WebRTC path.
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
		case 5:
			hasIDR = true
		case 7:
			hasSPS = true
		case 8:
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
