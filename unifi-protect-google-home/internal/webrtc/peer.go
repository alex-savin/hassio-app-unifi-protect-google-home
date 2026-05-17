// Package webrtc creates pion PeerConnections that consume a streams.Stream
// and deliver media to Google Home.
//
// Google CameraStream WebRTC flow (Cloud-to-Cloud):
//  1. Google calls EXECUTE action.devices.commands.GetCameraStream.
//  2. We return cameraStreamSignalingUrl + ICE servers + cameraStreamProtocol="webrtc".
//  3. Google POSTs an SDP offer to the signaling URL.
//  4. We create a pion PeerConnection, attach the stream's tracks, set the
//     remote description, create an answer, gather ICE, and return the answer.
//
// Trickle ICE is disabled — we wait for ICE gathering to complete and return
// a full SDP answer, which is the simplest mode Google accepts.
package webrtc

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	pion "github.com/pion/webrtc/v4"

	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/streams"
)

// SignalingOffer is the SDP offer JSON Google posts to the signaling URL.
type SignalingOffer struct {
	SDP  string `json:"sdp"`
	Type string `json:"type"` // usually "offer"
}

// SignalingAnswer is the SDP answer we return to Google.
type SignalingAnswer struct {
	SDP  string `json:"sdp"`
	Type string `json:"type"` // "answer"
}

// ICEServer is the JSON shape Google expects in cameraStreamIceServers.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// Factory builds Sessions on demand.
type Factory struct {
	ICEServers []ICEServer

	mu       sync.Mutex
	sessions []*Session
}

// NewFactory returns a Factory with a default Google STUN server.
func NewFactory() *Factory {
	return &Factory{
		ICEServers: []ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}
}

// Session is one active WebRTC PeerConnection.
type Session struct {
	pc      *pion.PeerConnection
	release func()
	closed  bool
	mu      sync.Mutex
}

// Negotiate accepts Google's offer, acquires the stream, attaches tracks,
// and returns the SDP answer (after ICE gathering completes).
func (f *Factory) Negotiate(ctx context.Context, s *streams.Stream, offer SignalingOffer) (*Session, SignalingAnswer, error) {
	if offer.SDP == "" {
		return nil, SignalingAnswer{}, errors.New("empty offer SDP")
	}

	cfg := pion.Configuration{
		ICEServers: toPionICE(f.ICEServers),
	}
	api := pion.NewAPI()
	pc, err := api.NewPeerConnection(cfg)
	if err != nil {
		return nil, SignalingAnswer{}, fmt.Errorf("new peer: %w", err)
	}

	tracks, release, err := s.Acquire(ctx)
	if err != nil {
		_ = pc.Close()
		return nil, SignalingAnswer{}, fmt.Errorf("acquire stream: %w", err)
	}
	for _, t := range tracks {
		if _, err := pc.AddTransceiverFromTrack(t,
			pion.RTPTransceiverInit{Direction: pion.RTPTransceiverDirectionSendonly},
		); err != nil {
			release()
			_ = pc.Close()
			return nil, SignalingAnswer{}, fmt.Errorf("add track: %w", err)
		}
	}

	sess := &Session{pc: pc, release: release}
	pc.OnConnectionStateChange(func(state pion.PeerConnectionState) {
		log.Printf("webrtc[%s] state=%s", s.Name, state.String())
		switch state {
		case pion.PeerConnectionStateFailed,
			pion.PeerConnectionStateClosed,
			pion.PeerConnectionStateDisconnected:
			sess.Close()
		}
	})

	if err := pc.SetRemoteDescription(pion.SessionDescription{
		Type: pion.SDPTypeOffer,
		SDP:  offer.SDP,
	}); err != nil {
		sess.Close()
		return nil, SignalingAnswer{}, fmt.Errorf("set remote: %w", err)
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		sess.Close()
		return nil, SignalingAnswer{}, fmt.Errorf("create answer: %w", err)
	}

	gathered := pion.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		sess.Close()
		return nil, SignalingAnswer{}, fmt.Errorf("set local: %w", err)
	}

	select {
	case <-gathered:
	case <-ctx.Done():
		sess.Close()
		return nil, SignalingAnswer{}, ctx.Err()
	}

	final := pc.LocalDescription()
	if final == nil {
		sess.Close()
		return nil, SignalingAnswer{}, errors.New("no local description after gather")
	}

	f.mu.Lock()
	f.sessions = append(f.sessions, sess)
	f.mu.Unlock()

	return sess, SignalingAnswer{SDP: final.SDP, Type: "answer"}, nil
}

// Close tears down the peer connection and releases the stream ref.
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.pc != nil {
		_ = s.pc.Close()
	}
	if s.release != nil {
		s.release()
	}
}

func toPionICE(in []ICEServer) []pion.ICEServer {
	out := make([]pion.ICEServer, 0, len(in))
	for _, s := range in {
		ps := pion.ICEServer{URLs: s.URLs}
		if s.Username != "" {
			ps.Username = s.Username
			ps.Credential = s.Credential
		}
		out = append(out, ps)
	}
	return out
}
