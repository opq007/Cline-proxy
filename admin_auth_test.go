package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Ensures admin middleware never blocks non-/admin paths even if mis-wired onto proxy routes.
func TestRequireAdminAuthSkipsNonAdminPaths(t *testing.T) {
	setAdminToken("secret-admin-only")
	t.Cleanup(func() { setAdminToken("") })

	called := false
	h := requireAdminAuth(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Client-facing paths must pass through without admin token.
	for _, path := range []string{
		"/v1/chat/completions",
		"/v1/messages",
		"/v1/models",
		"/v1/health",
		"/health",
		"/models",
		"/chat/completions",
		"/messages",
	} {
		called = false
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		// Deliberately no admin token / no API key headers.
		h(rr, req)
		if !called {
			t.Fatalf("requireAdminAuth blocked non-admin path %s (status=%d body=%s)", path, rr.Code, rr.Body.String())
		}
		if rr.Code != http.StatusOK {
			t.Fatalf("path %s: want 200, got %d", path, rr.Code)
		}
	}
}

func TestRequireAdminAuthEnforcesAdminAPI(t *testing.T) {
	setAdminToken("secret-admin-only")
	t.Cleanup(func() { setAdminToken("") })

	called := false
	h := requireAdminAuth(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/accounts", nil)
	h(rr, req)
	if called {
		t.Fatal("expected unauthenticated /admin/api request to be rejected")
	}
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d body=%s", rr.Code, rr.Body.String())
	}

	called = false
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin/api/accounts", nil)
	req.Header.Set("X-Admin-Token", "secret-admin-only")
	h(rr, req)
	if !called || rr.Code != http.StatusOK {
		t.Fatalf("valid admin token should pass: called=%v code=%d", called, rr.Code)
	}
}

func TestAdminTokenIndependentFromPoolKeys(t *testing.T) {
	// Admin token must not be confused with p.Keys used by apiKeyHandler.
	setAdminToken("admin-secret")
	t.Cleanup(func() { setAdminToken("") })

	if adminTokenMatches("pool-api-key-xyz") {
		t.Fatal("pool API key must not satisfy admin token check")
	}
	if !adminTokenMatches("admin-secret") {
		t.Fatal("admin token should match itself")
	}
	if getAdminToken() == "" {
		t.Fatal("admin token should be set")
	}
}

func TestAdminCookiePathIsAdminOnly(t *testing.T) {
	c := adminAuthCookie("tok", 3600)
	if c.Path != "/admin" {
		t.Fatalf("cookie Path must be /admin so it is not sent to /v1/*; got %q", c.Path)
	}
}
