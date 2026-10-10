package main

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestClientAddressUsesOnlyExistingTrustedTLSBoundary(t *testing.T) {
	trusted := netip.MustParsePrefix("10.203.0.1/32")
	for _, test := range []struct {
		name, peer, proto string
		values            []string
		direct            bool
		want              string
	}{
		{name: "trusted", peer: "10.203.0.1:1234", proto: "https", values: []string{"198.51.100.1"}, want: "198.51.100.1"},
		{name: "untrusted", peer: "203.0.113.1:1234", proto: "https", values: []string{"198.51.100.1"}, want: "203.0.113.1"},
		{name: "direct", peer: "10.203.0.1:1234", proto: "https", values: []string{"198.51.100.1"}, direct: true, want: "10.203.0.1"},
		{name: "not https", peer: "10.203.0.1:1234", proto: "http", values: []string{"198.51.100.1"}, want: "10.203.0.1"},
		{name: "missing", peer: "10.203.0.1:1234", proto: "https", want: "10.203.0.1"},
		{name: "multiple", peer: "10.203.0.1:1234", proto: "https", values: []string{"198.51.100.1", "198.51.100.2"}, want: "10.203.0.1"},
		{name: "list", peer: "10.203.0.1:1234", proto: "https", values: []string{"198.51.100.1, 198.51.100.2"}, want: "10.203.0.1"},
		{name: "invalid", peer: "10.203.0.1:1234", proto: "https", values: []string{"hostname.test"}, want: "10.203.0.1"},
		{name: "zone", peer: "10.203.0.1:1234", proto: "https", values: []string{"fe80::1%eth0"}, want: "10.203.0.1"},
		{name: "mapped", peer: "[::ffff:10.203.0.1]:1234", proto: "https", values: []string{"::ffff:198.51.100.1"}, want: "198.51.100.1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/login", nil)
			request.RemoteAddr = test.peer
			request.TLS = &tls.ConnectionState{}
			request.Header.Set("X-Forwarded-Proto", test.proto)
			for _, value := range test.values {
				request.Header.Add("CF-Connecting-IP", value)
			}
			request.Header.Set("X-Forwarded-For", "192.0.2.42")
			request.Header.Set("X-Real-IP", "192.0.2.43")
			request.Header.Set("Forwarded", "for=192.0.2.44")
			security := transportSecurity{directTLS: test.direct, trustedProxies: []netip.Prefix{trusted}}
			if got := security.clientAddress(request); got.String() != test.want {
				t.Fatalf("주소 = %v, want %s", got, test.want)
			}
			request.Header.Add("X-Forwarded-Proto", "https")
			if got := security.clientAddress(request); got != netip.MustParseAddr("203.0.113.1") && got != netip.MustParseAddr("10.203.0.1") {
				t.Fatal("복수 HTTPS 전달 값에서 헤더를 사용했습니다")
			}
		})
	}
}
