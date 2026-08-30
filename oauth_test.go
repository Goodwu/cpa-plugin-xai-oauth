package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func base64URLEncode(claims map[string]any) string {
	raw, err := json.Marshal(claims)
	if err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func newTestClient(server *httptest.Server) *oauthClient {
	client := newOAuthClient("")
	// Point discovery at the test server; the default fail-closed endpoint
	// validator stays active unless a test overrides it.
	client.httpClient = server.Client()
	client.discoveryOverride = server.URL + "/.well-known/openid-configuration"
	return client
}

func acceptAnyEndpoint(rawURL, field string) (string, error) {
	if strings.TrimSpace(rawURL) == "" {
		return "", fmt.Errorf("xai discovery %s is empty", field)
	}
	return rawURL, nil
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, payload any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func TestDiscoverResolvesAndValidatesEndpoints(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		writeJSON(t, w, http.StatusOK, discovery{
			DeviceAuthorizationEndpoint: "https://auth.x.ai/device/authorize",
			TokenEndpoint:               "https://auth.x.ai/token",
		})
	}))
	defer server.Close()

	client := newTestClient(server)
	resolved, err := client.discover(context.Background())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if resolved.DeviceAuthorizationEndpoint != "https://auth.x.ai/device/authorize" {
		t.Fatalf("unexpected device endpoint: %s", resolved.DeviceAuthorizationEndpoint)
	}
	if resolved.TokenEndpoint != "https://auth.x.ai/token" {
		t.Fatalf("unexpected token endpoint: %s", resolved.TokenEndpoint)
	}
}

func TestDiscoverRejectsOffIssuerEndpoints(t *testing.T) {
	for name, payload := range map[string]discovery{
		"non-xai-host": {DeviceAuthorizationEndpoint: "https://evil.example.com/device", TokenEndpoint: "https://auth.x.ai/token"},
		"plain-http":   {DeviceAuthorizationEndpoint: "http://auth.x.ai/device", TokenEndpoint: "https://auth.x.ai/token"},
		"missing":      {DeviceAuthorizationEndpoint: "", TokenEndpoint: "https://auth.x.ai/token"},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, http.StatusOK, payload)
		}))
		client := newTestClient(server)
		if _, err := client.discover(context.Background()); err == nil {
			t.Fatalf("%s: expected discovery to fail", name)
		}
		server.Close()
	}
}

func TestExchangeDeviceCodePendingAndSlowDown(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if got := r.PostForm.Get("grant_type"); got != deviceCodeGrantType {
			t.Fatalf("unexpected grant_type: %s", got)
		}
		if got := r.PostForm.Get("client_id"); got != oauthClientID {
			t.Fatalf("unexpected client_id: %s", got)
		}
		writeJSON(t, w, http.StatusBadRequest, map[string]string{"error": "authorization_pending"})
	}))
	defer server.Close()

	client := newTestClient(server)
	outcome := client.exchangeDeviceCode(context.Background(), server.URL+"/token", "device-code", defaultPollInterval)
	if outcome.Status != "pending" {
		t.Fatalf("expected pending, got %s (%s)", outcome.Status, outcome.Message)
	}

	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusBadRequest, map[string]string{"error": "slow_down"})
	})
	outcome = client.exchangeDeviceCode(context.Background(), server.URL+"/token", "device-code", defaultPollInterval)
	if outcome.Status != "slow_down" {
		t.Fatalf("expected slow_down, got %s", outcome.Status)
	}
	if outcome.NextInterval != defaultPollInterval+defaultPollInterval {
		t.Fatalf("unexpected slowed interval: %s", outcome.NextInterval)
	}
	_ = calls
}

func TestExchangeDeviceCodeSuccess(t *testing.T) {
	idToken := "header." + base64URLEncode(map[string]any{"email": "user@example.com", "sub": "sub-123"}) + ".sig"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"access_token":  "access-1",
			"refresh_token": "refresh-1",
			"id_token":      idToken,
			"token_type":    "bearer",
			"expires_in":    3600,
		})
	}))
	defer server.Close()

	client := newTestClient(server)
	outcome := client.exchangeDeviceCode(context.Background(), server.URL+"/token", "device-code", defaultPollInterval)
	if outcome.Status != "success" || outcome.Token == nil {
		t.Fatalf("expected success, got %s (%s)", outcome.Status, outcome.Message)
	}
	if outcome.Token.AccessToken != "access-1" || outcome.Token.RefreshToken != "refresh-1" {
		t.Fatalf("unexpected tokens: %+v", outcome.Token)
	}
	if outcome.Token.Email != "user@example.com" || outcome.Token.Subject != "sub-123" {
		t.Fatalf("unexpected identity: %s %s", outcome.Token.Email, outcome.Token.Subject)
	}
	if _, err := time.Parse(time.RFC3339, outcome.Token.Expire); err != nil {
		t.Fatalf("expired should be RFC3339: %v", err)
	}
}

func TestExchangeDeviceCodeTerminalErrors(t *testing.T) {
	cases := map[string]struct {
		payload map[string]string
		message string
	}{
		"expired": {map[string]string{"error": "expired_token"}, "expired"},
		"denied":  {map[string]string{"error": "access_denied"}, "denied"},
		"unknown": {map[string]string{"error": "other", "error_description": "boom"}, "other"},
	}
	for name, tc := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, http.StatusBadRequest, tc.payload)
		}))
		client := newTestClient(server)
		outcome := client.exchangeDeviceCode(context.Background(), server.URL+"/token", "device-code", defaultPollInterval)
		if outcome.Status != "error" {
			t.Fatalf("%s: expected error, got %s", name, outcome.Status)
		}
		if !strings.Contains(outcome.Message, tc.message) {
			t.Fatalf("%s: message %q should contain %q", name, outcome.Message, tc.message)
		}
		server.Close()
	}
}

func TestRefreshTokens(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if got := r.PostForm.Get("grant_type"); got != "refresh_token" {
			t.Fatalf("unexpected grant_type: %s", got)
		}
		if got := r.PostForm.Get("refresh_token"); got != "refresh-1" {
			t.Fatalf("unexpected refresh_token: %s", got)
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"access_token": "access-2",
			"expires_in":   7200,
		})
	}))
	defer server.Close()

	client := newTestClient(server)
	token, err := client.refreshTokens(context.Background(), " refresh-1 ", server.URL+"/token")
	if err != nil {
		t.Fatalf("refreshTokens: %v", err)
	}
	if token.AccessToken != "access-2" {
		t.Fatalf("unexpected access token: %s", token.AccessToken)
	}
	if _, err := client.refreshTokens(context.Background(), "", server.URL+"/token"); err == nil {
		t.Fatalf("expected error for empty refresh token")
	}
}
