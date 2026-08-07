package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestNormalizeProxyURLBareHost(t *testing.T) {
	got, err := normalizeProxyURL("127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://127.0.0.1:7890" {
		t.Fatalf("want http default scheme, got %q", got)
	}
}

func TestNormalizeProxyURLEmpty(t *testing.T) {
	got, err := normalizeProxyURL("  ")
	if err != nil || got != "" {
		t.Fatalf("empty should be direct: got %q err=%v", got, err)
	}
}

func TestNormalizeProxyURLStripsPath(t *testing.T) {
	got, err := normalizeProxyURL("http://127.0.0.1:7890/path?x=1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://127.0.0.1:7890" {
		t.Fatalf("path/query should be stripped: %q", got)
	}
}

func TestBuildProxiedTransportDisablesHTTP2(t *testing.T) {
	tr, err := buildProxiedTransport("http://127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	if tr.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 must be false for proxied clients")
	}
	if tr.TLSNextProto == nil {
		t.Fatal("TLSNextProto must be non-nil empty map to disable h2")
	}
	if tr.Proxy == nil {
		t.Fatal("http proxy should set Proxy func")
	}
}

func TestBuildSOCKSTransport(t *testing.T) {
	tr, err := buildProxiedTransport("socks5://127.0.0.1:1080")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Proxy != nil {
		t.Fatal("socks should not use HTTP Proxy")
	}
	if tr.DialContext == nil {
		t.Fatal("socks needs DialContext")
	}
}

func TestClassifyMalformedHTTPResponse(t *testing.T) {
	raw := errors.New(`Post "https://api.workos.com/x": net/http: HTTP/1.x transport connection broken: malformed HTTP response "\x00\x00\x12\x04"`)
	out := classifyProxyDialError(raw, "http://127.0.0.1:7891")
	s := out.Error()
	if !strings.Contains(s, "代理协议") && !strings.Contains(s, "端口不匹配") {
		t.Fatalf("expected actionable hint, got: %s", s)
	}
	if !strings.Contains(s, "socks5") {
		t.Fatalf("hint should mention socks5 alternative: %s", s)
	}
}

func TestClientForProxyURLCaches(t *testing.T) {
	// Isolate cache key
	u := "http://127.0.0.1:19999"
	c1, err := clientForProxyURL(u)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := clientForProxyURL(u)
	if err != nil {
		t.Fatal(err)
	}
	if c1 != c2 {
		t.Fatal("expected cached client reuse")
	}
	if c1 == httpClient {
		t.Fatal("proxied client must not be global direct client")
	}
}

func TestInvalidateProxyClient(t *testing.T) {
	u := "http://127.0.0.1:19998"
	c1, err := clientForProxyURL(u)
	if err != nil {
		t.Fatal(err)
	}
	invalidateProxyClient(u)
	c2, err := clientForProxyURL(u)
	if err != nil {
		t.Fatal(err)
	}
	if c1 == c2 {
		t.Fatal("after invalidate, client should be rebuilt")
	}
}

// Ensure direct client also has h2 disabled (shared base transport).
func TestDirectTransportNoForcedHTTP2(t *testing.T) {
	if httpTransport.ForceAttemptHTTP2 {
		t.Fatal("direct transport ForceAttemptHTTP2 should be false")
	}
	_ = http.MethodGet
}
