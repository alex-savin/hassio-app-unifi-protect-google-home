package discovery

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
)

// buildResponse assembles a UBNT response packet matching what a console
// would emit on the wire.
func buildResponse(version, command byte, fields []tlv) []byte {
	body := &bytes.Buffer{}
	for _, f := range fields {
		body.WriteByte(f.t)
		_ = binary.Write(body, binary.BigEndian, uint16(len(f.v)))
		body.Write(f.v)
	}
	var hdr [4]byte
	hdr[0] = version
	hdr[1] = command
	binary.BigEndian.PutUint16(hdr[2:], uint16(body.Len()))
	out := make([]byte, 0, 4+body.Len())
	out = append(out, hdr[:]...)
	out = append(out, body.Bytes()...)
	return out
}

type tlv struct {
	t byte
	v []byte
}

func TestParseResponse_V1(t *testing.T) {
	pkt := buildResponse(0x01, 0x00, []tlv{
		{t: fieldHWAddr, v: []byte{0xb4, 0xfb, 0xe4, 0x01, 0x02, 0x03}},
		{t: fieldHostname, v: []byte("unifi-dream-machine")},
		{t: fieldPlatform, v: []byte("UDMPRO")},
		{t: fieldFWVersion, v: []byte("3.2.12")},
		{t: fieldUptime, v: []byte{0x00, 0x00, 0x10, 0x00}},
	})
	from := &net.UDPAddr{IP: net.IPv4(192, 168, 1, 10), Port: Port}
	d, ok := parseResponse(pkt, from)
	if !ok {
		t.Fatalf("parse failed")
	}
	if d.SourceIP != "192.168.1.10" {
		t.Fatalf("SourceIP=%q", d.SourceIP)
	}
	if d.HWAddr != "b4:fb:e4:01:02:03" {
		t.Fatalf("HWAddr=%q", d.HWAddr)
	}
	if d.Hostname != "unifi-dream-machine" {
		t.Fatalf("Hostname=%q", d.Hostname)
	}
	if d.Platform != "UDMPRO" {
		t.Fatalf("Platform=%q", d.Platform)
	}
	if d.FWVersion != "3.2.12" {
		t.Fatalf("FWVersion=%q", d.FWVersion)
	}
	if d.Uptime != 0x1000 {
		t.Fatalf("Uptime=%d", d.Uptime)
	}
}

func TestParseResponse_V2WithProductAndVersion(t *testing.T) {
	pkt := buildResponse(0x02, 0x06, []tlv{
		{t: fieldHWAddr, v: []byte{0x68, 0xd7, 0x9a, 0xff, 0xee, 0xdd}},
		{t: fieldProductName, v: []byte("UDM-Pro")},
		{t: fieldVersion, v: []byte("3.2.12")},
	})
	from := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 5), Port: Port}
	d, ok := parseResponse(pkt, from)
	if !ok {
		t.Fatalf("parse failed")
	}
	if d.ProductName != "UDM-Pro" || d.Version != "3.2.12" {
		t.Fatalf("got %+v", d)
	}
}

func TestParseResponse_EchoFromEphemeralPort(t *testing.T) {
	// A console echoing our V1 request back from a high port.
	from := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 6), Port: 55123}
	d, ok := parseResponse(reqV1, from)
	if !ok {
		t.Fatalf("expected echo to parse")
	}
	if d.SourceIP != "10.0.0.6" {
		t.Fatalf("SourceIP=%q", d.SourceIP)
	}
}

func TestParseResponse_TooShort(t *testing.T) {
	from := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 6), Port: Port}
	if _, ok := parseResponse([]byte{1, 2}, from); ok {
		t.Fatalf("expected reject")
	}
}

func TestParseResponse_TruncatedTLV(t *testing.T) {
	// Length claims 20 bytes but body has only 2.
	pkt := []byte{0x01, 0x00, 0x00, 0x05, fieldHostname, 0x00, 0x14, 'a', 'b'}
	from := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 6), Port: Port}
	d, ok := parseResponse(pkt, from)
	if !ok {
		t.Fatalf("expected parse to succeed (with empty fields)")
	}
	if d.Hostname != "" {
		t.Fatalf("expected truncated TLV to be dropped, got %q", d.Hostname)
	}
}

func TestMerge(t *testing.T) {
	a := Device{SourceIP: "1.1.1.1", HWAddr: "aa:bb:cc:dd:ee:ff"}
	b := Device{SourceIP: "1.1.1.1", Hostname: "udm", Version: "3.0.0"}
	a.merge(b)
	if a.Hostname != "udm" || a.Version != "3.0.0" {
		t.Fatalf("merge missed fields: %+v", a)
	}
	if a.HWAddr != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("merge clobbered existing field")
	}
}
