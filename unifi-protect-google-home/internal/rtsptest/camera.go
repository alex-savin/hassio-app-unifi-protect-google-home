// Package rtsptest provides an in-process RTSPS camera for tests: a
// gortsplib server behind a self-signed TLS certificate that streams a
// synthetic H.264 feed, mirroring how UniFi Protect serves cameras
// (rtsps:// on TCP, parameter sets in the SDP, periodic IDRs).
package rtsptest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
)

// SPS and PPS are a valid 1920x1080 Baseline parameter-set pair.
var (
	SPS = []byte{
		0x67, 0x42, 0xc0, 0x28, 0xd9, 0x00, 0x78, 0x02,
		0x27, 0xe5, 0x84, 0x00, 0x00, 0x03, 0x00, 0x04,
		0x00, 0x00, 0x03, 0x00, 0xf0, 0x3c, 0x60, 0xc9,
		0x20,
	}
	PPS = []byte{0x68, 0xce, 0x3c, 0x80}
)

// Camera is a running fake camera.
type Camera struct {
	// URL is the rtsps:// URL to dial (certificate is self-signed, so
	// clients must skip verification).
	URL string
	// Plays counts PLAY requests served.
	Plays atomic.Int32

	server *gortsplib.Server
	stream *gortsplib.ServerStream
}

type handler struct{ cam *Camera }

func (h *handler) OnDescribe(*gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, h.cam.stream, nil
}

func (h *handler) OnSetup(*gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, h.cam.stream, nil
}

func (h *handler) OnPlay(*gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	h.cam.Plays.Add(1)
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// Start launches a camera on a random loopback port and streams ~30 fps
// H.264 (an IDR with in-band SPS/PPS every 15 frames) until the test ends.
func Start(t *testing.T) *Camera {
	t.Helper()
	cam := &Camera{}
	cam.server = &gortsplib.Server{
		Handler:     &handler{cam: cam},
		TLSConfig:   &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}},
		RTSPAddress: "127.0.0.1:0",
	}
	if err := cam.server.Start(); err != nil {
		t.Fatalf("rtsptest: server start: %v", err)
	}
	t.Cleanup(cam.server.Close)

	forma := &format.H264{PayloadTyp: 96, PacketizationMode: 1, SPS: SPS, PPS: PPS}
	media := &description.Media{Type: description.MediaTypeVideo, Formats: []format.Format{forma}}
	cam.stream = &gortsplib.ServerStream{
		Server: cam.server,
		Desc:   &description.Session{Medias: []*description.Media{media}},
	}
	if err := cam.stream.Initialize(); err != nil {
		t.Fatalf("rtsptest: stream init: %v", err)
	}
	t.Cleanup(cam.stream.Close)

	enc, err := forma.CreateEncoder()
	if err != nil {
		t.Fatalf("rtsptest: encoder: %v", err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	t.Cleanup(func() {
		close(stop)
		<-done
	})
	go func() {
		defer close(done)
		tick := time.NewTicker(time.Second / 30)
		defer tick.Stop()
		for frame := 0; ; frame++ {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			au := [][]byte{{0x41, 0x9a, byte(frame)}} // non-IDR slice
			if frame%15 == 0 {
				au = [][]byte{SPS, PPS, {0x65, 0x88, 0x84, byte(frame)}}
			}
			pkts, err := enc.Encode(au)
			if err != nil {
				t.Errorf("rtsptest: encode: %v", err)
				return
			}
			ts := uint32(frame * 3000) // 90 kHz / 30 fps
			for _, p := range pkts {
				p.Timestamp += ts
				_ = cam.stream.WritePacketRTP(media, p)
			}
		}
	}()

	cam.URL = "rtsps://" + cam.server.NetListener().Addr().String() + "/cam"
	return cam
}

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "rtsptest"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
