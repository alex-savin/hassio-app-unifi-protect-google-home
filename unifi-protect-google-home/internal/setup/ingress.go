package setup

import (
	"log"
	"net"
	"net/http"
	"net/netip"
)

// ingressProxyAddr is the fixed address of the Supervisor's ingress proxy
// on the hassio network. Every legitimate request to the setup UI arrives
// from it, already authenticated by Home Assistant.
var ingressProxyAddr = netip.MustParseAddr("172.30.32.2")

// IngressOnly rejects requests that did not come through Home Assistant
// ingress. The setup server listens on all interfaces (the ingress proxy
// reaches it over the hassio bridge network) and has no auth of its own,
// so without this any container on that network could rewrite the add-on
// options and restart it. Loopback is allowed for the Docker HEALTHCHECK.
func IngressOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !fromIngress(r.RemoteAddr) {
			log.Printf("setup: rejected request from %s (not ingress)", r.RemoteAddr)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func fromIngress(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	return ip == ingressProxyAddr || ip.IsLoopback()
}
