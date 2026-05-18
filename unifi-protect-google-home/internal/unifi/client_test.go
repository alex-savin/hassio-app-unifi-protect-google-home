package unifi

import (
	"encoding/json"
	"testing"
)

func TestIsDirectConnect(t *testing.T) {
	cases := map[string]bool{
		"abc123.ui.direct":      true,
		"ABC123.UI.DIRECT":      true,
		"abc.ui.direct:443":     true,
		"unifi.local":           false,
		"192.168.1.1":           false,
		"unifi.local.ui.direct": true,
		"":                      false,
	}
	for host, want := range cases {
		if got := IsDirectConnect(host); got != want {
			t.Errorf("IsDirectConnect(%q)=%v want %v", host, got, want)
		}
	}
}

func TestHasCloudAccount(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{`null`, false},
		{``, false},
		{`   null  `, false},
		{`{"email":"x@y.z"}`, true},
		{`{}`, true},
	}
	for _, tc := range cases {
		if got := hasCloudAccount(json.RawMessage(tc.raw)); got != tc.want {
			t.Errorf("hasCloudAccount(%q)=%v want %v", tc.raw, got, tc.want)
		}
	}
}
