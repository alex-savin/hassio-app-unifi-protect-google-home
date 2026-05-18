package unifi

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Event is a decoded change notification from the UniFi Protect updates WS.
type Event struct {
	Action      string                     // "add" | "update" | "remove"
	ModelKey    string                     // "camera", "event", "nvr", ...
	ID          string                     // entity id
	NewUpdateID string                     // controller's monotonically-increasing cursor
	Fields      map[string]json.RawMessage // for "update": only the changed fields; for "add": the full object
}

// SubscribeEvents opens the Protect updates WebSocket and emits decoded
// events. The channel is closed when ctx is canceled or the connection
// terminates; the second return value reports the terminating error (nil on
// clean ctx cancel).
//
// The Protect updates protocol frames each notification as two concatenated
// "packets" inside a single binary WS message:
//
//	[8-byte header][payload]   ; packet 1 (action frame, JSON)
//	[8-byte header][payload]   ; packet 2 (data frame, JSON)
//
// Header layout:
//
//	byte 0   : packet type   (1 = action, 2 = data)
//	byte 1   : payload format (1 = JSON)
//	byte 2   : deflated flag  (0 or 1; 1 = zlib-deflated payload)
//	byte 3   : unused
//	bytes 4-7: payload size, big-endian uint32
func (c *Client) SubscribeEvents(ctx context.Context, lastUpdateID string) (<-chan Event, error) {
	if err := c.ensureAuth(ctx); err != nil {
		return nil, err
	}

	u := url.URL{Scheme: "wss", Host: c.cfg.Host, Path: "/proxy/protect/ws/updates"}
	if lastUpdateID != "" {
		q := u.Query()
		q.Set("lastUpdateId", lastUpdateID)
		u.RawQuery = q.Encode()
	}

	hdr := http.Header{}
	c.mu.Lock()
	if c.csrfToken != "" {
		hdr.Set("X-CSRF-Token", c.csrfToken)
	}
	c.mu.Unlock()

	dialer := *websocket.DefaultDialer
	dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: !c.cfg.VerifyTLS} //nolint:gosec
	dialer.HandshakeTimeout = 15 * time.Second
	dialer.Jar = c.http.Jar

	conn, resp, err := dialer.DialContext(ctx, u.String(), hdr)
	if err != nil {
		if resp != nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			_ = resp.Body.Close()
			return nil, fmt.Errorf("ws dial: %s: %s", resp.Status, strings.TrimSpace(string(b)))
		}
		return nil, fmt.Errorf("ws dial: %w", err)
	}

	out := make(chan Event, 32)
	go func() {
		defer close(out)
		defer func() { _ = conn.Close() }()

		// Keep the connection alive with pings; reset read deadline on pong.
		const pongWait = 60 * time.Second
		const pingPeriod = 25 * time.Second
		_ = conn.SetReadDeadline(time.Now().Add(pongWait))
		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(pongWait))
		})

		pingDone := make(chan struct{})
		go func() {
			t := time.NewTicker(pingPeriod)
			defer t.Stop()
			for {
				select {
				case <-pingDone:
					return
				case <-ctx.Done():
					return
				case <-t.C:
					_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
				}
			}
		}()
		defer close(pingDone)

		// Close the conn when ctx is canceled to unblock ReadMessage.
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			select {
			case <-ctx.Done():
				_ = conn.Close()
			case <-stop:
			}
		}()

		for {
			mt, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if mt != websocket.BinaryMessage {
				continue
			}
			ev, ok, err := decodeEvent(payload)
			if err != nil {
				// Malformed frame — skip but keep the socket alive.
				continue
			}
			if !ok {
				continue
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()

	return out, nil
}

type wsAction struct {
	Action      string `json:"action"`
	NewUpdateID string `json:"newUpdateId"`
	ModelKey    string `json:"modelKey"`
	ID          string `json:"id"`
}

func decodeEvent(buf []byte) (Event, bool, error) {
	rdr := bytes.NewReader(buf)

	var act wsAction
	var data map[string]json.RawMessage

	for i := 0; i < 2; i++ {
		var hdr [8]byte
		if _, err := io.ReadFull(rdr, hdr[:]); err != nil {
			return Event{}, false, fmt.Errorf("read header: %w", err)
		}
		pktType := hdr[0]
		deflated := hdr[2] != 0
		size := binary.BigEndian.Uint32(hdr[4:8])
		if size > 4<<20 {
			return Event{}, false, fmt.Errorf("packet too large: %d", size)
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(rdr, payload); err != nil {
			return Event{}, false, fmt.Errorf("read payload: %w", err)
		}
		if deflated {
			zr, err := zlib.NewReader(bytes.NewReader(payload))
			if err != nil {
				return Event{}, false, fmt.Errorf("zlib: %w", err)
			}
			payload, err = io.ReadAll(zr)
			_ = zr.Close()
			if err != nil {
				return Event{}, false, fmt.Errorf("zlib read: %w", err)
			}
		}

		switch pktType {
		case 1: // action
			if err := json.Unmarshal(payload, &act); err != nil {
				return Event{}, false, fmt.Errorf("action json: %w", err)
			}
		case 2: // data
			if len(payload) > 0 && payload[0] == '{' {
				if err := json.Unmarshal(payload, &data); err != nil {
					return Event{}, false, fmt.Errorf("data json: %w", err)
				}
			}
		}
	}

	if act.ModelKey == "" {
		return Event{}, false, nil
	}
	return Event{
		Action:      act.Action,
		ModelKey:    act.ModelKey,
		ID:          act.ID,
		NewUpdateID: act.NewUpdateID,
		Fields:      data,
	}, true, nil
}
