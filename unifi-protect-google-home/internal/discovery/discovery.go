// Package discovery implements the UBNT UDP discovery protocol used to find
// UniFi Protect consoles on the local network. It speaks both the legacy V1
// and the newer V2 packet formats, broadcasts to 255.255.255.255:10001 and
// (best-effort) joins the UniFi multicast group 233.89.188.1:10001 so that
// consoles which only answer multicast are still found.
//
// Wire format (all integers big-endian):
//
//	header: version (u8) | command (u8) | data_len (u16)
//	fields: type (u8) | length (u16) | value (length bytes)
//
// Reference: https://github.com/uilibs/unifi-discovery
package discovery

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

const (
	// Port is the UBNT discovery UDP port.
	Port = 10001

	// MulticastIP is the address consoles use when responding to clients
	// that join the UniFi multicast group.
	MulticastIP = "233.89.188.1"

	protoV1 = 0x01
	protoV2 = 0x02

	cmdV1Discovery  = 0x00
	cmdV2Request    = 0x08
	cmdV2ResponseA  = 0x06
	cmdV2ResponseB  = 0x09

	fieldHWAddr      = 0x01
	fieldIPInfo      = 0x02
	fieldFWVersion   = 0x03
	fieldAddrEntry   = 0x04
	fieldMACAddress  = 0x05
	fieldUptime      = 0x0A
	fieldHostname    = 0x0B
	fieldPlatform    = 0x0C
	fieldModel       = 0x14
	fieldProductName = 0x15
	fieldVersion     = 0x16
)

// reqV1 / reqV2 are the only two request packets we send.
var (
	reqV1 = []byte{protoV1, cmdV1Discovery, 0x00, 0x00}
	reqV2 = []byte{protoV2, cmdV2Request, 0x00, 0x00}
)

// Device is a single UBNT discovery result. Most fields are best-effort —
// older firmware omits some.
type Device struct {
	SourceIP    string `json:"source_ip"`
	HWAddr      string `json:"hw_addr,omitempty"`
	Hostname    string `json:"hostname,omitempty"`
	Platform    string `json:"platform,omitempty"`
	Model       string `json:"model,omitempty"`
	ProductName string `json:"product_name,omitempty"`
	Version     string `json:"version,omitempty"`
	FWVersion   string `json:"fw_version,omitempty"`
	Uptime      uint32 `json:"uptime,omitempty"`
}

// merge fills empty fields in d from other.
func (d *Device) merge(other Device) {
	if d.HWAddr == "" {
		d.HWAddr = other.HWAddr
	}
	if d.Hostname == "" {
		d.Hostname = other.Hostname
	}
	if d.Platform == "" {
		d.Platform = other.Platform
	}
	if d.Model == "" {
		d.Model = other.Model
	}
	if d.ProductName == "" {
		d.ProductName = other.ProductName
	}
	if d.Version == "" {
		d.Version = other.Version
	}
	if d.FWVersion == "" {
		d.FWVersion = other.FWVersion
	}
	if d.Uptime == 0 {
		d.Uptime = other.Uptime
	}
}

// Scan broadcasts UBNT V1+V2 discovery requests and collects responses until
// timeout elapses. Returns the deduplicated device list keyed by source IP.
func Scan(ctx context.Context, timeout time.Duration) ([]Device, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 0})
	if err != nil {
		return nil, fmt.Errorf("listen udp: %w", err)
	}
	defer func() { _ = conn.Close() }()

	bcast := &net.UDPAddr{IP: net.IPv4bcast, Port: Port}
	mcast := &net.UDPAddr{IP: net.ParseIP(MulticastIP), Port: Port}

	send := func() {
		_, _ = conn.WriteToUDP(reqV1, bcast)
		_, _ = conn.WriteToUDP(reqV2, bcast)
		_, _ = conn.WriteToUDP(reqV1, mcast)
		_, _ = conn.WriteToUDP(reqV2, mcast)
	}
	send()

	// Retransmit every third of the budget — packets are easily lost.
	retry := time.NewTicker(timeout / 3)
	defer retry.Stop()

	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetReadDeadline(deadline)

	devices := map[string]Device{}
	buf := make([]byte, 2048)
	for {
		// Re-send on tick (non-blocking) before reading.
		select {
		case <-retry.C:
			send()
		default:
		}

		n, from, rerr := conn.ReadFromUDP(buf)
		if rerr != nil {
			var nerr net.Error
			if errors.As(rerr, &nerr) && nerr.Timeout() {
				break
			}
			return nil, fmt.Errorf("read: %w", rerr)
		}
		if ctx.Err() != nil {
			break
		}
		dev, ok := parseResponse(buf[:n], from)
		if !ok {
			continue
		}
		if existing, had := devices[dev.SourceIP]; had {
			existing.merge(dev)
			devices[dev.SourceIP] = existing
		} else {
			devices[dev.SourceIP] = dev
		}
	}

	out := make([]Device, 0, len(devices))
	for _, d := range devices {
		out = append(out, d)
	}
	return out, nil
}

// parseResponse decodes a UBNT reply into a Device. The boolean is false when
// the payload is not a recognised response (e.g. our own echoed request).
func parseResponse(payload []byte, from *net.UDPAddr) (Device, bool) {
	if len(payload) < 4 || from == nil {
		return Device{}, false
	}
	// Filter out our own broadcast echoing back from the kernel.
	if from.Port != Port && payload[0] == protoV1 && payload[1] == cmdV1Discovery {
		// A console echoing our V1 request from an ephemeral port is a UniFi
		// OS device signal, but it carries no fields — surface just the IP.
		return Device{SourceIP: from.IP.String()}, true
	}

	version := payload[0]
	command := payload[1]
	dataLen := int(binary.BigEndian.Uint16(payload[2:4]))

	// Accept (V1, cmd 0) and (V2, cmd 6|9). V0 fallback: any command with
	// data_len > 0 (observed on some UNVR firmware).
	switch {
	case version == protoV1 && command == cmdV1Discovery:
	case version == protoV2 && (command == cmdV2ResponseA || command == cmdV2ResponseB):
	case version == 0x00 && dataLen > 0:
	default:
		return Device{}, false
	}

	body := payload[4:]
	if dataLen > len(body) {
		dataLen = len(body)
	}
	body = body[:dataLen]

	d := Device{SourceIP: from.IP.String()}
	for len(body) >= 3 {
		ft := body[0]
		fl := int(binary.BigEndian.Uint16(body[1:3]))
		if 3+fl > len(body) {
			break
		}
		val := body[3 : 3+fl]
		switch ft {
		case fieldHWAddr, fieldMACAddress:
			if fl == 6 && d.HWAddr == "" {
				d.HWAddr = formatMAC(val)
			}
		case fieldIPInfo:
			// MAC(6) + IP(4); use the IP as a fallback only.
			if fl == 10 && d.HWAddr == "" {
				d.HWAddr = formatMAC(val[:6])
			}
		case fieldFWVersion:
			d.FWVersion = string(val)
		case fieldUptime:
			if fl == 4 {
				d.Uptime = binary.BigEndian.Uint32(val)
			}
		case fieldHostname:
			d.Hostname = string(val)
		case fieldPlatform:
			d.Platform = string(val)
		case fieldModel:
			d.Model = string(val)
		case fieldProductName:
			d.ProductName = string(val)
		case fieldVersion:
			d.Version = string(val)
		case fieldAddrEntry:
			// IP address; ignore.
		}
		body = body[3+fl:]
	}
	return d, true
}

func formatMAC(b []byte) string {
	var sb strings.Builder
	sb.Grow(17)
	for i, c := range b {
		if i > 0 {
			sb.WriteByte(':')
		}
		const hex = "0123456789abcdef"
		sb.WriteByte(hex[c>>4])
		sb.WriteByte(hex[c&0x0f])
	}
	return sb.String()
}
