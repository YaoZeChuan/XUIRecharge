package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"xui-recharge/internal/config"
	"xui-recharge/internal/database"
	"xui-recharge/internal/panel"
	"xui-recharge/internal/service"
)

func TestStaticPagesHealthAndAdminCSRF(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "recharge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := config.Config{
		Server:   config.ServerConfig{Host: "127.0.0.1", Port: 8080, Timezone: "Asia/Shanghai"},
		Database: config.DatabaseConfig{Path: filepath.Join(t.TempDir(), "recharge.db")},
		Panel:    config.PanelConfig{Mode: "mock", TimeoutSeconds: 5, TLSVerify: true},
		Recharge: config.RechargeConfig{AllowExpiredRenewal: true, MaxDaysPerOrder: 3650, MaxTrafficGBPerOrder: 10000},
		Security: config.SecurityConfig{MaxFailedAttempts: 5, AdminSessionHours: 12, CardHMACSecret: []byte(strings.Repeat("c", 64)), SessionSecret: []byte(strings.Repeat("s", 64)), AdminUsername: "admin", AdminInitialPassword: "valid-test-admin-password"},
	}
	app := service.New(db, panel.NewMock(), cfg)
	if err := app.EnsureInitialAdmin(context.Background()); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := NewRouter(app, cfg, logger)
	for _, target := range []string{"/", "/admin", "/assets/css/app.css", "/assets/js/user.js"} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d", target, response.Code)
		}
	}
	health := httptest.NewRecorder()
	router.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/api/healthz", nil))
	if health.Code != http.StatusOK || !strings.Contains(health.Body.String(), `"status":"ok"`) {
		t.Fatalf("health response = %d %s", health.Code, health.Body.String())
	}
	unauthorized := httptest.NewRecorder()
	router.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/admin/dashboard", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated admin API returned %d", unauthorized.Code)
	}

	loginBody := []byte(`{"username":"admin","password":"valid-test-admin-password"}`)
	loginRequest := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", bytes.NewReader(loginBody))
	loginRequest.Header.Set("Content-Type", "application/json")
	loginResponse := httptest.NewRecorder()
	router.ServeHTTP(loginResponse, loginRequest)
	if loginResponse.Code != http.StatusOK {
		t.Fatalf("login failed: %d %s", loginResponse.Code, loginResponse.Body.String())
	}
	var decoded struct {
		Data struct {
			CSRFToken string `json:"csrfToken"`
		} `json:"data"`
	}
	if err := json.Unmarshal(loginResponse.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	var sessionCookie, csrfCookie *http.Cookie
	for _, cookie := range loginResponse.Result().Cookies() {
		if cookie.Name == "admin_session" {
			sessionCookie = cookie
		}
		if cookie.Name == "admin_csrf" {
			csrfCookie = cookie
		}
	}
	if sessionCookie == nil || csrfCookie == nil || decoded.Data.CSRFToken == "" {
		t.Fatal("login did not establish session and CSRF cookies")
	}

	writePackage := func(csrf string) *httptest.ResponseRecorder {
		body := []byte(`{"name":"7 days","type":"TIME","durationDays":7,"trafficBytes":0,"enabled":true}`)
		request := httptest.NewRequest(http.MethodPost, "/api/admin/packages", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.AddCookie(sessionCookie)
		request.AddCookie(csrfCookie)
		if csrf != "" {
			request.Header.Set("X-CSRF-Token", csrf)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response
	}
	if response := writePackage(""); response.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF header returned %d", response.Code)
	}
	if response := writePackage(decoded.Data.CSRFToken); response.Code != http.StatusOK {
		t.Fatalf("valid admin request returned %d: %s", response.Code, response.Body.String())
	}
}
