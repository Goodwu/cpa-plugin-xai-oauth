package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// pinClientFactory returns a factory that routes all plugin HTTP traffic to server.
func pinClientFactory(server *httptest.Server) func(hostConfigSummary) *oauthClient {
	return func(host hostConfigSummary) *oauthClient {
		client := newOAuthClient(host.ProxyURL)
		client.httpClient = server.Client()
		client.discoveryOverride = server.URL + "/.well-known/openid-configuration"
		return client
	}
}

func TestLoginStartAndPollFlow(t *testing.T) {
	idToken := "header." + base64URLEncode(map[string]any{"email": "user@example.com", "sub": "sub-123"}) + ".sig"
	var polls int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/.well-known/openid-configuration"):
			writeJSON(t, w, http.StatusOK, discovery{
				DeviceAuthorizationEndpoint: "http://" + r.Host + "/device/authorize",
				TokenEndpoint:               "http://" + r.Host + "/token",
			})
		case strings.HasSuffix(r.URL.Path, "/device/authorize"):
			writeJSON(t, w, http.StatusOK, deviceCodeResponse{
				DeviceCode:              "device-code-1",
				UserCode:                "ABCD-1234",
				VerificationURIComplete: "https://auth.x.ai/device?code=ABCD-1234",
				ExpiresIn:               600,
			})
		default:
			if atomic.AddInt64(&polls, 1) == 1 {
				writeJSON(t, w, http.StatusBadRequest, map[string]string{"error": "authorization_pending"})
				return
			}
			writeJSON(t, w, http.StatusOK, map[string]any{
				"access_token":  "access-1",
				"refresh_token": "refresh-1",
				"id_token":      idToken,
				"token_type":    "bearer",
				"expires_in":    3600,
			})
		}
	}))
	defer server.Close()

	original := newOAuthClientFor
	factory := pinClientFactory(server)
	newOAuthClientFor = func(host hostConfigSummary) *oauthClient {
		client := factory(host)
		client.validateEndpoint = acceptAnyEndpoint
		return client
	}
	defer func() { newOAuthClientFor = original }()
	setDefaultPollInterval(time.Nanosecond)
	defer setDefaultPollInterval(5 * time.Second)

	startRequest, _ := json.Marshal(loginStartRequest{Provider: authProviderID})
	response, err := dispatch("auth.login.start", startRequest)
	if err != nil {
		t.Fatalf("dispatch start: %v", err)
	}
	var started loginStartResponse
	if err := json.Unmarshal(envelopeResult(t, response), &started); err != nil {
		t.Fatalf("decode start: %v", err)
	}
	if started.Provider != authProviderID || started.State == "" {
		t.Fatalf("unexpected start response: %+v", started)
	}
	if !strings.Contains(started.URL, "auth.x.ai") {
		t.Fatalf("unexpected login URL: %s", started.URL)
	}
	if started.Metadata["login_device_code"] != "device-code-1" {
		t.Fatalf("metadata should carry the device code: %+v", started.Metadata)
	}

	// First poll hits authorization_pending.
	pollRequest, _ := json.Marshal(loginPollRequest{Provider: authProviderID, State: started.State, Metadata: started.Metadata})
	response, err = dispatch("auth.login.poll", pollRequest)
	if err != nil {
		t.Fatalf("dispatch poll: %v", err)
	}
	var pending loginPollResponse
	if err := json.Unmarshal(envelopeResult(t, response), &pending); err != nil {
		t.Fatalf("decode poll: %v", err)
	}
	if pending.Status != "pending" {
		t.Fatalf("expected pending, got %s (%s)", pending.Status, pending.Message)
	}

	// Second poll completes the flow.
	response, err = dispatch("auth.login.poll", pollRequest)
	if err != nil {
		t.Fatalf("dispatch poll 2: %v", err)
	}
	var completed loginPollResponse
	if err := json.Unmarshal(envelopeResult(t, response), &completed); err != nil {
		t.Fatalf("decode poll 2: %v", err)
	}
	if completed.Status != "success" {
		t.Fatalf("expected success, got %s (%s)", completed.Status, completed.Message)
	}
	if completed.Auth.FileName != "xai-oauth-user@example.com.json" {
		t.Fatalf("unexpected auth file name: %s", completed.Auth.FileName)
	}
	if completed.Auth.Attributes["api_key"] != "access-1" {
		t.Fatalf("auth attributes should carry the access token")
	}

	// The in-memory session is consumed after success; further polls of the
	// same state would rebuild from metadata but the device code is
	// single-use upstream, so the host must stop polling once completed.
	if _, ok := sessions.get(started.State); ok {
		t.Fatalf("session should be consumed after login success")
	}
}

func TestLoginPollRebuildsSessionFromMetadata(t *testing.T) {
	idToken := "header." + base64URLEncode(map[string]any{"email": "user@example.com"}) + ".sig"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/token"):
			writeJSON(t, w, http.StatusOK, map[string]any{
				"access_token":  "access-1",
				"refresh_token": "refresh-1",
				"id_token":      idToken,
				"expires_in":    3600,
			})
		default:
			writeJSON(t, w, http.StatusOK, discovery{
				DeviceAuthorizationEndpoint: "http://" + r.Host + "/device/authorize",
				TokenEndpoint:               "http://" + r.Host + "/token",
			})
		}
	}))
	defer server.Close()

	original := newOAuthClientFor
	factory := pinClientFactory(server)
	newOAuthClientFor = func(host hostConfigSummary) *oauthClient {
		client := factory(host)
		client.validateEndpoint = acceptAnyEndpoint
		return client
	}
	defer func() { newOAuthClientFor = original }()
	setDefaultPollInterval(time.Nanosecond)
	defer setDefaultPollInterval(5 * time.Second)

	metadata := map[string]any{
		"login_device_code":      "device-code-2",
		"login_token_endpoint":   server.URL + "/token",
		"login_interval_seconds": "1",
		"login_expires_at":       time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
	}
	pollRequest, _ := json.Marshal(loginPollRequest{Provider: authProviderID, State: "restored-state", Metadata: metadata})
	response, err := dispatch("auth.login.poll", pollRequest)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	var completed loginPollResponse
	if err := json.Unmarshal(envelopeResult(t, response), &completed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if completed.Status != "success" {
		t.Fatalf("restored session should complete, got %s (%s)", completed.Status, completed.Message)
	}
}
