package unifi

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"testing"
)

// pack builds a Protect-style packet (header + payload, optionally zlib-deflated).
func pack(pktType byte, deflated bool, payload []byte) []byte {
	body := payload
	if deflated {
		var buf bytes.Buffer
		zw := zlib.NewWriter(&buf)
		_, _ = zw.Write(payload)
		_ = zw.Close()
		body = buf.Bytes()
	}
	var hdr [8]byte
	hdr[0] = pktType
	hdr[1] = 1 // JSON
	if deflated {
		hdr[2] = 1
	}
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(body)))
	out := make([]byte, 0, 8+len(body))
	out = append(out, hdr[:]...)
	out = append(out, body...)
	return out
}

func TestDecodeEvent_UpdatePlain(t *testing.T) {
	action := []byte(`{"action":"update","newUpdateId":"abc","modelKey":"camera","id":"cam1"}`)
	data := []byte(`{"state":"CONNECTED"}`)
	buf := append(pack(1, false, action), pack(2, false, data)...)

	ev, ok, err := decodeEvent(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !ok {
		t.Fatal("expected ok")
	}
	if ev.Action != "update" || ev.ModelKey != "camera" || ev.ID != "cam1" || ev.NewUpdateID != "abc" {
		t.Fatalf("bad action frame: %+v", ev)
	}
	if _, has := ev.Fields["state"]; !has {
		t.Fatalf("missing state field: %+v", ev.Fields)
	}
}

func TestDecodeEvent_Deflated(t *testing.T) {
	action := []byte(`{"action":"add","newUpdateId":"x","modelKey":"camera","id":"cam9"}`)
	data := []byte(`{"id":"cam9","name":"Driveway","state":"CONNECTED"}`)
	buf := append(pack(1, true, action), pack(2, true, data)...)

	ev, ok, err := decodeEvent(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !ok || ev.Action != "add" || ev.ID != "cam9" {
		t.Fatalf("bad event: %+v ok=%v", ev, ok)
	}
	if string(ev.Fields["name"]) != `"Driveway"` {
		t.Fatalf("bad name field: %s", ev.Fields["name"])
	}
}

func TestDecodeEvent_RemoveNoData(t *testing.T) {
	action := []byte(`{"action":"remove","newUpdateId":"y","modelKey":"camera","id":"cam2"}`)
	// "remove" still ships a data packet, but it can be empty bytes.
	buf := append(pack(1, false, action), pack(2, false, []byte{})...)

	ev, ok, err := decodeEvent(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !ok || ev.Action != "remove" || ev.ID != "cam2" {
		t.Fatalf("bad event: %+v ok=%v", ev, ok)
	}
	if ev.Fields != nil {
		t.Fatalf("expected nil fields, got %v", ev.Fields)
	}
}
