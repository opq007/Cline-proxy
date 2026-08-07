package main

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Admin access token protects ONLY the management console:
//
//	/admin/  (UI shell is public; SPA prompts for token)
//	/admin/api/* business routes (via requireAdminAuth)
//
// It MUST NOT be applied to Cline client / proxy traffic:
//
//	/v1/chat/completions, /v1/messages, /v1/models, /health, ...
//
// Those routes keep the existing pool API-Key middleware (apiKeyHandler in proxy.go).
// Do not wrap mux-wide handlers or call requireAdminAuth from proxy routes.

const adminTokenCookie = "cline_admin_token"

var (
	adminTokenMu sync.RWMutex
	adminToken   string // empty = admin auth disabled (local/dev)
)

// initAdminToken resolves the admin access token.
// Priority: CLI flag > env ADMIN_TOKEN / CLINE_ADMIN_TOKEN > .cline-accounts.json adminToken.
func initAdminToken(flagValue string) {
	tok := strings.TrimSpace(flagValue)
	if tok == "" {
		tok = strings.TrimSpace(os.Getenv("ADMIN_TOKEN"))
	}
	if tok == "" {
		tok = strings.TrimSpace(os.Getenv("CLINE_ADMIN_TOKEN"))
	}
	if tok == "" {
		// loadPool may not be ready yet; read config field after pool load
		p := loadPool()
		if p != nil {
			tok = strings.TrimSpace(p.AdminToken)
		}
	}
	setAdminToken(tok)
	if tok == "" {
		log.Printf("WARNING: admin token not set — /admin is OPEN. Set ADMIN_TOKEN / -admin-token / config adminToken for public deploy.")
	} else {
		log.Printf("Admin auth enabled (token length=%d)", len(tok))
	}
}

func setAdminToken(tok string) {
	adminTokenMu.Lock()
	adminToken = strings.TrimSpace(tok)
	adminTokenMu.Unlock()
}

func getAdminToken() string {
	adminTokenMu.RLock()
	defer adminTokenMu.RUnlock()
	return adminToken
}

func adminAuthRequired() bool {
	return getAdminToken() != ""
}

func adminTokenMatches(provided string) bool {
	expected := getAdminToken()
	if expected == "" {
		return true
	}
	provided = strings.TrimSpace(provided)
	if provided == "" {
		return false
	}
	// constant-time compare; lengths may differ — pad via subtle on equal-length only
	if len(provided) != len(expected) {
		// still do a dummy compare to reduce trivial timing leak on length
		_ = subtle.ConstantTimeCompare([]byte(expected), []byte(expected))
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func extractAdminCredential(r *http.Request) string {
	if h := r.Header.Get("X-Admin-Token"); h != "" {
		return strings.TrimSpace(h)
	}
	if auth := r.Header.Get("Authorization"); auth != "" {
		const bearer = "Bearer "
		if len(auth) > len(bearer) && strings.EqualFold(auth[:len(bearer)], bearer) {
			return strings.TrimSpace(auth[len(bearer):])
		}
		// also accept raw token in Authorization
		return strings.TrimSpace(auth)
	}
	if c, err := r.Cookie(adminTokenCookie); err == nil && c != nil {
		return strings.TrimSpace(c.Value)
	}
	return ""
}

func isAdminAuthenticated(r *http.Request) bool {
	if !adminAuthRequired() {
		return true
	}
	return adminTokenMatches(extractAdminCredential(r))
}

// requireAdminAuth wraps /admin/api/* handlers only.
// Never use this middleware on /v1/* or other client-facing proxy endpoints.
func requireAdminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Defense-in-depth: refuse to enforce on non-admin paths if mis-wired.
		if !strings.HasPrefix(r.URL.Path, "/admin/") {
			next(w, r)
			return
		}
		if !adminAuthRequired() {
			next(w, r)
			return
		}
		if isAdminAuthenticated(r) {
			next(w, r)
			return
		}
		writeAPI(w, http.StatusUnauthorized, apiResponse{
			Error: "unauthorized: admin token required (Authorization: Bearer <token> or X-Admin-Token)",
		})
	}
}

func adminAuthCookie(token string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     adminTokenCookie,
		Value:    token,
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
		// Secure left false so plain HTTP LAN deploys still work; put TLS in front when public.
	}
}

// GET /admin/api/auth/status — public
func handleAdminAuthStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	required := adminAuthRequired()
	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Data: map[string]any{
			"required":      required,
			"authenticated": isAdminAuthenticated(r),
		},
	})
}

// POST /admin/api/auth/login — public body: { "token": "..." }
func handleAdminAuthLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	if !adminAuthRequired() {
		writeAPI(w, http.StatusOK, apiResponse{
			Success: true,
			Message: "admin auth not required",
			Data:    map[string]any{"required": false, "authenticated": true},
		})
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPI(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	defer r.Body.Close()

	var req struct {
		Token string `json:"token"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeAPI(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON"})
			return
		}
	}
	tok := strings.TrimSpace(req.Token)
	if tok == "" {
		tok = extractAdminCredential(r)
	}
	if !adminTokenMatches(tok) {
		// mild delay against brute force
		time.Sleep(200 * time.Millisecond)
		writeAPI(w, http.StatusUnauthorized, apiResponse{Error: "invalid admin token"})
		return
	}

	http.SetCookie(w, adminAuthCookie(tok, 7*24*3600))
	writeAPI(w, http.StatusOK, apiResponse{
		Success: true,
		Message: "logged in",
		Data:    map[string]any{"required": true, "authenticated": true},
	})
}

// POST /admin/api/auth/logout — clear cookie
func handleAdminAuthLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeAPI(w, http.StatusMethodNotAllowed, apiResponse{Error: "method not allowed"})
		return
	}
	http.SetCookie(w, adminAuthCookie("", -1))
	writeAPI(w, http.StatusOK, apiResponse{Success: true, Message: "logged out"})
}
