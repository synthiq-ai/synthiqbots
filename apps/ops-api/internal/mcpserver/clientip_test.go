package mcpserver

import (
	"net/http"
	"testing"
)

func TestClientIPXForwardedFor(t *testing.T) {
	r := &http.Request{Header: http.Header{}, RemoteAddr: "10.0.0.1:51000"}
	r.Header.Set("X-Forwarded-For", "203.0.113.42, 10.0.0.99")
	if got := clientIP(r); got != "203.0.113.42" {
		t.Errorf("XFF first entry expected, got %q", got)
	}
}

func TestClientIPXRealIP(t *testing.T) {
	r := &http.Request{Header: http.Header{}, RemoteAddr: "10.0.0.1:51000"}
	r.Header.Set("X-Real-IP", "203.0.113.7")
	if got := clientIP(r); got != "203.0.113.7" {
		t.Errorf("X-Real-IP expected, got %q", got)
	}
}

func TestClientIPCloudflare(t *testing.T) {
	r := &http.Request{Header: http.Header{}, RemoteAddr: "10.0.0.1:51000"}
	r.Header.Set("Cf-Connecting-IP", "198.51.100.5")
	if got := clientIP(r); got != "198.51.100.5" {
		t.Errorf("Cf-Connecting-IP expected, got %q", got)
	}
}

func TestClientIPRemoteAddrFallback(t *testing.T) {
	r := &http.Request{Header: http.Header{}, RemoteAddr: "203.0.113.10:443"}
	if got := clientIP(r); got != "203.0.113.10" {
		t.Errorf("RemoteAddr-with-port expected stripped, got %q", got)
	}
}

func TestClientIPJunkRemoteAddr(t *testing.T) {
	// httptest / some Go test paths set RemoteAddr to "client" (no colon, no IP).
	// The pre-fix code carried that string straight into audit rows.
	r := &http.Request{Header: http.Header{}, RemoteAddr: "client"}
	if got := clientIP(r); got != "unknown" {
		t.Errorf("junk RemoteAddr should fall back to 'unknown', got %q", got)
	}
}

func TestClientIPIPv6Bracketed(t *testing.T) {
	r := &http.Request{Header: http.Header{}, RemoteAddr: "[2001:db8::1]:51000"}
	if got := clientIP(r); got != "2001:db8::1" {
		t.Errorf("bracketed IPv6 should strip brackets+port, got %q", got)
	}
}

func TestClientIPXFFWithJunkFirst(t *testing.T) {
	// Defensive: if some odd proxy puts "client" in XFF, skip it and find the next valid IP.
	r := &http.Request{Header: http.Header{}, RemoteAddr: "10.0.0.1:51000"}
	r.Header.Set("X-Forwarded-For", "client, 203.0.113.99")
	if got := clientIP(r); got != "203.0.113.99" {
		t.Errorf("expected to skip junk and pick next valid IP, got %q", got)
	}
}
