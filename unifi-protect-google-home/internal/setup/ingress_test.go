package setup

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIngressOnly(t *testing.T) {
	h := IngressOnly(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	cases := []struct {
		remote string
		want   int
	}{
		{"172.30.32.2:51000", http.StatusOK},
		{"[::ffff:172.30.32.2]:51000", http.StatusOK},
		{"127.0.0.1:40000", http.StatusOK},
		{"[::1]:40000", http.StatusOK},
		{"172.30.32.5:51000", http.StatusForbidden},
		{"192.168.1.20:51000", http.StatusForbidden},
		{"garbage", http.StatusForbidden},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, "/api/save", nil)
		req.RemoteAddr = c.remote
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != c.want {
			t.Errorf("%s: status=%d want %d", c.remote, rr.Code, c.want)
		}
	}
}
