package webrtc

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pion/sdp/v3"
	pion "github.com/pion/webrtc/v4"

	"github.com/alex-savin/hassio-app-unifi-protect-google-home/internal/streams"
)

// fakeProducer mimics the rtsp.Producer track layout without a network
// source: one H.264 sample track, High profile fmtp, like a Protect camera.
// It counts Start/Stop so tests can check the stream ref is released.
type fakeProducer struct {
	starts atomic.Int32
	stops  atomic.Int32
}

func (f *fakeProducer) Start(_ context.Context) ([]pion.TrackLocal, error) {
	f.starts.Add(1)
	track, err := pion.NewTrackLocalStaticSample(
		pion.RTPCodecCapability{
			MimeType:    pion.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640028",
		},
		"video", "cam-test",
	)
	if err != nil {
		return nil, err
	}
	return []pion.TrackLocal{track}, nil
}

func (f *fakeProducer) Stop() error {
	f.stops.Add(1)
	return nil
}

// googleStyleOffer approximates the offer a Nest display sends for a
// CameraStream WebRTC session: BUNDLE, audio (Opus, sendrecv for talkback),
// video (H.264, recvonly), and an application data channel.
const googleStyleOffer = "v=0\r\n" +
	"o=- 4611731400430051336 2 IN IP4 127.0.0.1\r\n" +
	"s=-\r\n" +
	"t=0 0\r\n" +
	"a=group:BUNDLE 0 1 2\r\n" +
	"a=extmap-allow-mixed\r\n" +
	"a=msid-semantic: WMS\r\n" +
	"m=audio 9 UDP/TLS/RTP/SAVPF 111 0 8\r\n" +
	"c=IN IP4 0.0.0.0\r\n" +
	"a=rtcp:9 IN IP4 0.0.0.0\r\n" +
	"a=ice-ufrag:4ZcD\r\n" +
	"a=ice-pwd:2/1muCWoOi3uLifh0NuRHlpf\r\n" +
	"a=ice-options:trickle\r\n" +
	"a=fingerprint:sha-256 7B:8B:F0:65:5F:78:E2:51:3B:AC:6F:F3:3F:46:1B:35:DC:B8:5F:64:1A:24:C2:43:F0:A1:58:D0:A1:2C:19:08\r\n" +
	"a=setup:actpass\r\n" +
	"a=mid:0\r\n" +
	"a=sendrecv\r\n" +
	"a=rtcp-mux\r\n" +
	"a=rtpmap:111 opus/48000/2\r\n" +
	"a=fmtp:111 minptime=10;useinbandfec=1\r\n" +
	"a=rtpmap:0 PCMU/8000\r\n" +
	"a=rtpmap:8 PCMA/8000\r\n" +
	"m=video 9 UDP/TLS/RTP/SAVPF 96 97 102 103\r\n" +
	"c=IN IP4 0.0.0.0\r\n" +
	"a=rtcp:9 IN IP4 0.0.0.0\r\n" +
	"a=ice-ufrag:4ZcD\r\n" +
	"a=ice-pwd:2/1muCWoOi3uLifh0NuRHlpf\r\n" +
	"a=ice-options:trickle\r\n" +
	"a=fingerprint:sha-256 7B:8B:F0:65:5F:78:E2:51:3B:AC:6F:F3:3F:46:1B:35:DC:B8:5F:64:1A:24:C2:43:F0:A1:58:D0:A1:2C:19:08\r\n" +
	"a=setup:actpass\r\n" +
	"a=mid:1\r\n" +
	"a=recvonly\r\n" +
	"a=rtcp-mux\r\n" +
	"a=rtcp-rsize\r\n" +
	"a=rtpmap:96 VP8/90000\r\n" +
	"a=rtcp-fb:96 nack\r\n" +
	"a=rtcp-fb:96 nack pli\r\n" +
	"a=rtpmap:97 rtx/90000\r\n" +
	"a=fmtp:97 apt=96\r\n" +
	"a=rtpmap:102 H264/90000\r\n" +
	"a=rtcp-fb:102 nack\r\n" +
	"a=rtcp-fb:102 nack pli\r\n" +
	"a=fmtp:102 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f\r\n" +
	"a=rtpmap:103 rtx/90000\r\n" +
	"a=fmtp:103 apt=102\r\n" +
	"m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n" +
	"c=IN IP4 0.0.0.0\r\n" +
	"a=ice-ufrag:4ZcD\r\n" +
	"a=ice-pwd:2/1muCWoOi3uLifh0NuRHlpf\r\n" +
	"a=ice-options:trickle\r\n" +
	"a=fingerprint:sha-256 7B:8B:F0:65:5F:78:E2:51:3B:AC:6F:F3:3F:46:1B:35:DC:B8:5F:64:1A:24:C2:43:F0:A1:58:D0:A1:2C:19:08\r\n" +
	"a=setup:actpass\r\n" +
	"a=mid:2\r\n" +
	"a=sctp-port:5000\r\n" +
	"a=max-message-size:262144\r\n"

// offlineFactory has no STUN servers: host candidates only, keeps the
// tests offline.
func offlineFactory() *Factory {
	f := NewFactory()
	f.ICEServers = nil
	return f
}

// TestNegotiateAnswerShape locks the answer properties a Nest display
// needs to play the stream. A pion upgrade or a change to Negotiate that
// breaks any of them shows up here instead of as a black tile in the
// Google Home app.
func TestNegotiateAnswerShape(t *testing.T) {
	prod := &fakeProducer{}
	stream := streams.NewStream("cam-test", prod)
	sess, answer, err := offlineFactory().Negotiate(context.Background(), stream, SignalingOffer{SDP: googleStyleOffer, Type: "offer"})
	if err != nil {
		t.Fatalf("negotiate: %v", err)
	}
	defer sess.Close()

	if answer.Type != "answer" {
		t.Errorf("answer type = %q, want %q", answer.Type, "answer")
	}
	parsed, err := (&pion.SessionDescription{Type: pion.SDPTypeAnswer, SDP: answer.SDP}).Unmarshal()
	if err != nil {
		t.Fatalf("answer SDP does not parse: %v\n%s", err, answer.SDP)
	}
	if got, _ := parsed.Attribute("group"); got != "BUNDLE 0 1 2" {
		t.Errorf("BUNDLE group = %q, want %q", got, "BUNDLE 0 1 2")
	}

	// Every offered m-line must be answered in order, same mid, and not
	// rejected (port 0) — a rejected m-line in a BUNDLE breaks the session.
	wantKinds := []string{"audio", "video", "application"}
	if len(parsed.MediaDescriptions) != len(wantKinds) {
		t.Fatalf("answer has %d m-lines, want %d\n%s", len(parsed.MediaDescriptions), len(wantKinds), answer.SDP)
	}
	for i, md := range parsed.MediaDescriptions {
		if md.MediaName.Media != wantKinds[i] {
			t.Errorf("m-line %d kind = %q, want %q", i, md.MediaName.Media, wantKinds[i])
		}
		if md.MediaName.Port.Value == 0 {
			t.Errorf("m-line %d (%s) rejected (port 0)", i, md.MediaName.Media)
		}
		if mid, _ := md.Attribute("mid"); mid != string(rune('0'+i)) {
			t.Errorf("m-line %d mid = %q, want %d", i, mid, i)
		}
		// The answerer must pick a DTLS role; echoing actpass is invalid.
		if setup, _ := md.Attribute("setup"); setup != "active" && setup != "passive" {
			t.Errorf("m-line %d setup = %q, want active or passive", i, setup)
		}
	}

	audio, video := parsed.MediaDescriptions[0], parsed.MediaDescriptions[1]

	// The bridge never sends audio (no camera audio track); talkback from
	// the display is accepted but not consumed.
	if dir := direction(audio.Attributes); dir != "recvonly" {
		t.Errorf("audio direction = %q, want recvonly", dir)
	}

	// Video: send-only, carrying the camera track, with an H.264 payload
	// in packetization-mode 1 (the only mode the Protect feed uses).
	if dir := direction(video.Attributes); dir != "sendonly" {
		t.Errorf("video direction = %q, want sendonly", dir)
	}
	if msid, _ := video.Attribute("msid"); !strings.HasPrefix(msid, "cam-test ") {
		t.Errorf("video msid = %q, want the camera track (cam-test ...)", msid)
	}
	h264PT := ""
	for _, a := range video.Attributes {
		if a.Key == "rtpmap" && strings.HasSuffix(a.Value, " H264/90000") {
			h264PT = strings.Fields(a.Value)[0]
		}
	}
	if h264PT == "" {
		t.Fatalf("video answer offers no H264 payload\n%s", answer.SDP)
	}
	var fmtp string
	for _, a := range video.Attributes {
		if a.Key == "fmtp" && strings.HasPrefix(a.Value, h264PT+" ") {
			fmtp = a.Value
		}
	}
	if !strings.Contains(fmtp, "packetization-mode=1") {
		t.Errorf("H264 fmtp = %q, want packetization-mode=1", fmtp)
	}

	// Trickle ICE is disabled: the answer must already carry candidates.
	if !strings.Contains(answer.SDP, "a=candidate:") {
		t.Errorf("answer carries no ICE candidates\n%s", answer.SDP)
	}

	if n := prod.starts.Load(); n != 1 {
		t.Errorf("producer started %d times, want 1", n)
	}
}

// TestSessionCloseReleasesStream: closing the session must release the
// stream ref so the upstream RTSP session to Protect is torn down; Close
// must be idempotent (the pion state callback and the caller both call it).
func TestSessionCloseReleasesStream(t *testing.T) {
	prod := &fakeProducer{}
	stream := streams.NewStream("cam-test", prod)
	sess, _, err := offlineFactory().Negotiate(context.Background(), stream, SignalingOffer{SDP: googleStyleOffer, Type: "offer"})
	if err != nil {
		t.Fatalf("negotiate: %v", err)
	}
	if n := prod.stops.Load(); n != 0 {
		t.Fatalf("producer stopped while session open (%d)", n)
	}
	sess.Close()
	sess.Close()
	if n := prod.stops.Load(); n != 1 {
		t.Fatalf("producer stopped %d times after Close, want 1", n)
	}
}

// TestNegotiateFailureReleasesStream: a bad offer must fail and must not
// leak the stream ref acquired before the remote description was applied.
func TestNegotiateFailureReleasesStream(t *testing.T) {
	cases := map[string]string{
		"empty":   "",
		"garbage": "this is not sdp",
	}
	for name, sdp := range cases {
		t.Run(name, func(t *testing.T) {
			prod := &fakeProducer{}
			stream := streams.NewStream("cam-test", prod)
			sess, _, err := offlineFactory().Negotiate(context.Background(), stream, SignalingOffer{SDP: sdp, Type: "offer"})
			if err == nil {
				sess.Close()
				t.Fatal("negotiate accepted an invalid offer")
			}
			if starts, stops := prod.starts.Load(), prod.stops.Load(); starts != stops {
				t.Fatalf("stream ref leaked: %d starts, %d stops", starts, stops)
			}
		})
	}
}

// direction returns the m-line's media direction attribute.
func direction(attrs []sdp.Attribute) string {
	for _, a := range attrs {
		switch a.Key {
		case "sendrecv", "sendonly", "recvonly", "inactive":
			return a.Key
		}
	}
	return ""
}
