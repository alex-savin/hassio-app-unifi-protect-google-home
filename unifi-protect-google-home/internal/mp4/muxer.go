// Package mp4 provides an on-demand RTSP→fragmented-MP4 muxer per camera.
//
// Google's Smart Home `CameraStream` trait supports several streaming
// protocols, but in practice only two are reliably consumed by every
// surface: WebRTC (Cast / Hub Max) and progressive_mp4 (phone Home app
// and most web/mobile clients). HLS is technically listed in the enum but
// the phone Home app refuses to request it from a cloud-to-cloud action,
// which is also why the Scrypted google-home plugin advertises only
// ["progressive_mp4", "webrtc"].
//
// progressive_mp4 in this context is a single never-ending HTTP response
// that carries a fragmented MP4: one ftyp+moov initialization segment
// followed by an unbounded sequence of moof+mdat fragments, written and
// flushed as upstream video is received. Each camera gets its own Muxer
// that:
//
//  1. Opens an RTSPS session to UniFi Protect on first subscriber.
//  2. Demuxes H.264 access units and builds fMP4 fragments.
//  3. Fans the init + every fragment out to all current HTTP subscribers.
//  4. Auto-closes upstream after an idle period and lazily restarts.
//
// Audio is intentionally dropped to keep the muxer simple and to dodge
// AAC timestamping quirks in some Protect firmwares. Phone tile playback
// is silent — that is consistent with how Nest Cam previews behave.
package mp4

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v4"
	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/bluenviron/gortsplib/v4/pkg/description"
	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"
	mp4codecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/pion/rtp"
)

const (
	// idleTimeout is how long the upstream RTSP session stays open after
	// the last HTTP subscriber disconnects. Short enough to free Protect
	// resources promptly; long enough that a quick reload doesn't pay the
	// reconnect cost.
	idleTimeout = 30 * time.Second

	// h264Timescale is the standard 90 kHz clock H.264 RTP uses.
	h264Timescale = 90000

	// subChanCap is the per-subscriber backlog of fMP4 fragments. If a
	// subscriber's HTTP writer can't keep up we drop fragments rather
	// than blocking the demux goroutine, which would stall every other
	// viewer of the same camera. 64 fragments at ~30 fps ≈ 2 s of slack.
	subChanCap = 64
)

// Source resolves a camera ID to the upstream RTSP URL. Returning
// ok=false means the camera is unknown.
type Source func(camID string) (rtspURL string, verifyTLS bool, ok bool)

// Muxer is one camera's progressive_mp4 pipeline. Safe for concurrent
// ServeHTTP from multiple subscribers (e.g. when both a phone and the
// Home web app view the same tile).
type Muxer struct {
	StreamID  string
	RTSPURL   string
	VerifyTLS bool

	mu         sync.Mutex
	started    bool
	initBytes  []byte
	rtspClient *gortsplib.Client
	cancelUp   context.CancelFunc
	subs       map[chan []byte]struct{}

	lastAccessNs atomic.Int64
}

// timedAU is one decoded access unit plus its RTP timestamp, handed from
// the RTP callback to the demux goroutine. All other muxing state lives as
// locals of runDemux, so the RTP callback never has to take m.mu —
// gortsplib's Client.Close blocks until in-flight callbacks return, and
// stopLocked calls Close while holding m.mu, so a callback that locked
// m.mu would deadlock.
type timedAU struct {
	ts uint32
	au [][]byte
}

// NewMuxer returns an idle Muxer for the given camera.
func NewMuxer(streamID, rtspURL string, verifyTLS bool) *Muxer {
	return &Muxer{
		StreamID:  streamID,
		RTSPURL:   rtspURL,
		VerifyTLS: verifyTLS,
		subs:      map[chan []byte]struct{}{},
	}
}

// ServeHTTP attaches the request as a subscriber. It writes the init
// segment, then streams fMP4 fragments until the client disconnects or
// the upstream session ends.
func (m *Muxer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.lastAccessNs.Store(time.Now().UnixNano())
	if err := m.ensureStarted(); err != nil {
		log.Printf("mp4[%s] start: %v", m.StreamID, err)
		http.Error(w, "stream unavailable", http.StatusServiceUnavailable)
		return
	}

	// Register before waiting for init so the session can never stop
	// without closing our channel: stopLocked closes every registered
	// subscriber, and registration refuses sessions that already stopped.
	ch := make(chan []byte, subChanCap)
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		http.Error(w, "stream restarting", http.StatusServiceUnavailable)
		return
	}
	m.subs[ch] = struct{}{}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.subs, ch)
		m.mu.Unlock()
	}()

	// Wait briefly for the init segment to be built (gortsplib has to
	// receive an IDR and pull SPS/PPS).
	initBytes, err := m.waitInit(r.Context(), 5*time.Second)
	if err != nil {
		http.Error(w, "stream warming up", http.StatusServiceUnavailable)
		return
	}

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Connection", "close")
	w.WriteHeader(http.StatusOK)

	if _, err := w.Write(initBytes); err != nil {
		return
	}
	if flusher != nil {
		flusher.Flush()
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case b, ok := <-ch:
			if !ok {
				return
			}
			if _, err := w.Write(b); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

// waitInit blocks until the init segment is available or ctx/timeout
// expires. The init segment is built once SPS/PPS are known.
func (m *Muxer) waitInit(ctx context.Context, timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	for {
		m.mu.Lock()
		b := m.initBytes
		m.mu.Unlock()
		if b != nil {
			return b, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return nil, errors.New("init segment not ready")
		}
	}
}

// Close stops upstream and disconnects all subscribers. Safe to call
// repeatedly.
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
	for ch := range m.subs {
		close(ch)
		delete(m.subs, ch)
	}
	m.initBytes = nil
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
			if h, ok := forma.(*format.H264); ok {
				videoMedia = medi
				videoFormat = h
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

	// The callback only decodes and hands off; see timedAU for why it
	// must not touch m.mu. Drop-on-full keeps a stalled demux goroutine
	// from ever blocking the RTSP read loop.
	auCh := make(chan timedAU, subChanCap)
	c.OnPacketRTP(videoMedia, videoFormat, func(pkt *rtp.Packet) {
		au, err := rtpDec.Decode(pkt)
		if err != nil {
			// Incomplete fragments and decode hiccups alike: skip and
			// resync on a later packet.
			return
		}
		select {
		case auCh <- timedAU{ts: pkt.Timestamp, au: cloneAU(au)}:
		default:
			// demux backed up; drop — subscribers recover at the next IDR.
		}
	})

	if _, err := c.Play(nil); err != nil {
		c.Close()
		return fmt.Errorf("play: %w", err)
	}

	upCtx, cancel := context.WithCancel(context.Background())
	m.cancelUp = cancel
	m.rtspClient = c
	m.started = true
	m.lastAccessNs.Store(time.Now().UnixNano())

	go m.runDemux(upCtx, auCh,
		append([]byte(nil), videoFormat.SPS...),
		append([]byte(nil), videoFormat.PPS...))
	go m.runReadLoop(upCtx, c)
	go m.runIdleWatchdog(upCtx)

	log.Printf("mp4[%s] started (progressive fMP4, %d-byte SPS)", m.StreamID, len(videoFormat.SPS))
	return nil
}

// runDemux owns all per-session muxing state as locals, so nothing here
// races with stopLocked. It buffers one AU so its Duration can be
// backfilled once the next AU arrives, then emits a single-sample fMP4
// Part to every subscriber. The init segment is built as soon as SPS/PPS
// are known — from the SDP, or harvested in-band for cameras that omit
// sprop-parameter-sets.
func (m *Muxer) runDemux(ctx context.Context, auCh <-chan timedAU, sps, pps []byte) {
	var (
		initBuilt    bool
		haveFirstTS  bool
		lastRawTS    uint32
		pts          int64
		pendingAU    [][]byte
		pendingPTS   int64
		pendingIsIDR bool
		havePending  bool
		baseTime     uint64
		seqNum       uint32
	)
	for {
		var tau timedAU
		select {
		case <-ctx.Done():
			return
		case tau = <-auCh:
		}

		for _, nalu := range tau.au {
			if len(nalu) == 0 {
				continue
			}
			switch h264.NALUType(nalu[0] & 0x1F) {
			case h264.NALUTypeSPS:
				sps = append(sps[:0], nalu...)
			case h264.NALUTypePPS:
				pps = append(pps[:0], nalu...)
			}
		}

		au := injectParamSets(tau.au, sps, pps)

		// Accumulating signed deltas keeps the PTS monotonic across the
		// 32-bit RTP timestamp wrap (~6.6 h at 90 kHz).
		if !haveFirstTS {
			lastRawTS = tau.ts
			haveFirstTS = true
		}
		pts += int64(int32(tau.ts - lastRawTS)) //nolint:gosec
		lastRawTS = tau.ts

		if !initBuilt {
			if len(sps) == 0 || len(pps) == 0 {
				continue // can't build moov until parameter sets arrive
			}
			if err := m.publishInit(ctx, sps, pps); err != nil {
				log.Printf("mp4[%s] build init: %v", m.StreamID, err)
				continue
			}
			initBuilt = true
		}

		isIDR := containsIDR(au)
		if !havePending {
			pendingAU, pendingPTS, pendingIsIDR = au, pts, isIDR
			havePending = true
			continue
		}

		// Backfill duration from the gap to this AU.
		duration := pts - pendingPTS
		if duration <= 0 {
			// Out-of-order or duplicate PTS; assume 1/30 s.
			duration = h264Timescale / 30
		}

		seqNum++
		if err := m.emitFragment(ctx, pendingAU, baseTime, seqNum, uint32(duration), pendingIsIDR); err != nil {
			log.Printf("mp4[%s] emit: %v", m.StreamID, err)
		}
		baseTime += uint64(duration)
		pendingAU, pendingPTS, pendingIsIDR = au, pts, isIDR
	}
}

// publishInit constructs the ftyp+moov initialization segment and makes it
// visible to waitInit, unless the session was stopped in the meantime.
func (m *Muxer) publishInit(ctx context.Context, sps, pps []byte) error {
	init := &fmp4.Init{
		Tracks: []*fmp4.InitTrack{
			{
				ID:        1,
				TimeScale: h264Timescale,
				Codec: &mp4codecs.H264{
					SPS: append([]byte(nil), sps...),
					PPS: append([]byte(nil), pps...),
				},
			},
		},
	}
	var buf seekablebuffer.Buffer
	if err := init.Marshal(&buf); err != nil {
		return fmt.Errorf("marshal init: %w", err)
	}
	m.mu.Lock()
	if ctx.Err() == nil {
		m.initBytes = buf.Bytes()
	}
	m.mu.Unlock()
	return nil
}

// emitFragment serialises one access unit as a fMP4 Part (moof+mdat) and
// fans it out to every current subscriber. Drops the fragment for slow
// subscribers rather than blocking — they'll recover from the next IDR.
// stopLocked cancels ctx while holding m.mu, so checking ctx under m.mu
// guarantees a straggling fragment can never reach a newer session's
// subscribers.
func (m *Muxer) emitFragment(ctx context.Context, au [][]byte, baseTime uint64, seqNum, duration uint32, isIDR bool) error {
	sample := &fmp4.Sample{}
	if err := sample.FillH264(0, au); err != nil {
		return fmt.Errorf("fill sample: %w", err)
	}
	sample.Duration = duration
	sample.IsNonSyncSample = !isIDR

	part := &fmp4.Part{
		SequenceNumber: seqNum,
		Tracks: []*fmp4.PartTrack{
			{
				ID:       1,
				BaseTime: baseTime,
				Samples:  []*fmp4.Sample{sample},
			},
		},
	}
	var buf seekablebuffer.Buffer
	if err := part.Marshal(&buf); err != nil {
		return fmt.Errorf("marshal part: %w", err)
	}
	data := buf.Bytes()

	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil {
		return nil
	}
	for ch := range m.subs {
		select {
		case ch <- data:
		default:
			// subscriber backed up; drop.
		}
	}
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
			log.Printf("mp4[%s] rtsp session ended: %v", m.StreamID, err)
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
			m.mu.Lock()
			hasSubs := len(m.subs) > 0
			m.mu.Unlock()
			if hasSubs {
				m.lastAccessNs.Store(time.Now().UnixNano())
				continue
			}
			last := time.Unix(0, m.lastAccessNs.Load())
			if time.Since(last) <= idleTimeout {
				continue
			}
			m.mu.Lock()
			stopped := false
			if m.started && len(m.subs) == 0 &&
				time.Since(time.Unix(0, m.lastAccessNs.Load())) > idleTimeout {
				log.Printf("mp4[%s] idle %s — stopping upstream", m.StreamID, idleTimeout)
				m.stopLocked()
				stopped = true
			}
			m.mu.Unlock()
			// Only exit once the session is actually stopped — a subscriber
			// may have re-attached between the unlocked check and the lock,
			// in which case this watchdog must keep watching.
			if stopped {
				return
			}
		}
	}
}

// injectParamSets prepends SPS/PPS to the access unit ahead of an IDR if
// they're not already inline. Mirrors the WebRTC and HLS paths.
func injectParamSets(au [][]byte, sps, pps []byte) [][]byte {
	if len(sps) == 0 || len(pps) == 0 {
		return au
	}
	hasIDR, hasSPS, hasPPS := false, false, false
	for _, nalu := range au {
		if len(nalu) == 0 {
			continue
		}
		switch h264.NALUType(nalu[0] & 0x1F) {
		case h264.NALUTypeIDR:
			hasIDR = true
		case h264.NALUTypeSPS:
			hasSPS = true
		case h264.NALUTypePPS:
			hasPPS = true
		}
	}
	if !hasIDR {
		return au
	}
	out := make([][]byte, 0, len(au)+2)
	if !hasSPS {
		out = append(out, sps)
	}
	if !hasPPS {
		out = append(out, pps)
	}
	out = append(out, au...)
	return out
}

func containsIDR(au [][]byte) bool {
	for _, nalu := range au {
		if len(nalu) > 0 && h264.NALUType(nalu[0]&0x1F) == h264.NALUTypeIDR {
			return true
		}
	}
	return false
}

func cloneAU(au [][]byte) [][]byte {
	out := make([][]byte, len(au))
	for i, n := range au {
		out[i] = append([]byte(nil), n...)
	}
	return out
}

// Server is a thin per-camera demultiplexer. It owns one Muxer per camera
// and lazily creates them on first request.
type Server struct {
	LookupRTSP Source

	mu      sync.Mutex
	muxers  map[string]*Muxer
	closing atomic.Bool
}

// NewServer returns a ready-to-use Server backed by the given lookup.
func NewServer(lookup Source) *Server {
	return &Server{LookupRTSP: lookup, muxers: map[string]*Muxer{}}
}

// ServeHTTP routes /<camID>/stream.mp4 to that camera's Muxer. The /mp4/
// prefix is expected to be stripped by the caller. Anything other than
// a known camID with a "stream.mp4" suffix returns 404.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.closing.Load() {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	// Path layout after prefix strip: /<camID>/stream.mp4
	path := r.URL.Path
	if len(path) > 0 && path[0] == '/' {
		path = path[1:]
	}
	idx := bytes.IndexByte([]byte(path), '/')
	if idx <= 0 {
		http.Error(w, "missing camera id", http.StatusBadRequest)
		return
	}
	camID := path[:idx]
	rest := path[idx+1:]
	if rest != "stream.mp4" {
		http.NotFound(w, r)
		return
	}
	mux := s.muxerFor(camID)
	if mux == nil {
		http.NotFound(w, r)
		return
	}
	mux.ServeHTTP(w, r)
}

func (s *Server) muxerFor(camID string) *Muxer {
	s.mu.Lock()
	defer s.mu.Unlock()
	if mux, ok := s.muxers[camID]; ok {
		return mux
	}
	url, verify, ok := s.LookupRTSP(camID)
	if !ok {
		return nil
	}
	mux := NewMuxer(camID, url, verify)
	s.muxers[camID] = mux
	return mux
}

// Shutdown closes every active Muxer.
func (s *Server) Shutdown() {
	s.closing.Store(true)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, mux := range s.muxers {
		mux.Close()
	}
	s.muxers = map[string]*Muxer{}
}
