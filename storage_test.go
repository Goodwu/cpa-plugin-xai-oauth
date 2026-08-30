package main

import (
	"encoding/json"
	"testing"
	"time"
)

func sampleToken() *tokenData {
	return &tokenData{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		IDToken:      "id-1",
		TokenType:    "bearer",
		ExpiresIn:    3600,
		Expire:       time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		Email:        "user@example.com",
		Subject:      "sub-123",
	}
}

func TestBuildAuthDataDefaultsToChatProxy(t *testing.T) {
	data, err := buildAuthData(buildTokenStorage(sampleToken(), "https://auth.x.ai/token"), "xai-oauth-user@example.com.json")
	if err != nil {
		t.Fatalf("buildAuthData: %v", err)
	}
	if data.Provider != pluginID {
		t.Fatalf("unexpected provider: %s", data.Provider)
	}
	if data.Attributes["base_url"] != cliChatProxyBaseURL {
		t.Fatalf("expected chat proxy base URL, got %s", data.Attributes["base_url"])
	}
	if data.Attributes["api_key"] != "access-1" {
		t.Fatalf("expected access token as api_key, got %q", data.Attributes["api_key"])
	}
	if data.Attributes["auth_kind"] != "oauth" {
		t.Fatalf("expected oauth auth kind")
	}
	if data.Attributes["using_api"] != "false" {
		t.Fatalf("expected using_api false, got %s", data.Attributes["using_api"])
	}
	if data.Attributes["header:"+tokenAuthHeader] != tokenAuthValue {
		t.Fatalf("missing CLI token auth header attribute")
	}
	if data.Attributes["header:"+clientVersionHeader] != grokClientVersion {
		t.Fatalf("missing CLI version header attribute")
	}
	if data.FileName != "xai-oauth-user@example.com.json" {
		t.Fatalf("unexpected file name: %s", data.FileName)
	}
	next, err := time.Parse(time.RFC3339, data.NextRefreshAfter)
	if err != nil {
		t.Fatalf("NextRefreshAfter should be RFC3339: %v", err)
	}
	if delta := time.Until(next); delta > 55*time.Minute || delta < 50*time.Minute {
		t.Fatalf("unexpected refresh lead: %s", delta)
	}
	var storage tokenStorage
	if err := json.Unmarshal(data.StorageJSON, &storage); err != nil {
		t.Fatalf("decode storage: %v", err)
	}
	if storage.Type != pluginID {
		t.Fatalf("storage type should be plugin id, got %s", storage.Type)
	}
	if storage.TokenEndpoint != "https://auth.x.ai/token" {
		t.Fatalf("unexpected token endpoint: %s", storage.TokenEndpoint)
	}
	if storage.AuthKind != "oauth" {
		t.Fatalf("unexpected auth kind: %s", storage.AuthKind)
	}
}

func TestUsingAPIVariants(t *testing.T) {
	storage := buildTokenStorage(sampleToken(), "")
	if storage.usingAPI() {
		t.Fatalf("default should not use api.x.ai")
	}
	storage.UsingAPI = true
	if !storage.usingAPI() {
		t.Fatalf("boolean true should enable api.x.ai")
	}
	storage.UsingAPI = "true"
	if !storage.usingAPI() {
		t.Fatalf("string true should enable api.x.ai")
	}
	if base := authBaseURL(storage, true); base != defaultAPIBaseURL {
		t.Fatalf("using_api should route to api.x.ai, got %s", base)
	}
	storage.BaseURL = "https://proxy.example.com/v1"
	if base := authBaseURL(storage, false); base != "https://proxy.example.com/v1" {
		t.Fatalf("explicit custom base URL should win, got %s", base)
	}
}

func TestCredentialFileName(t *testing.T) {
	if got := credentialFileName("user@example.com", ""); got != "xai-oauth-user@example.com.json" {
		t.Fatalf("unexpected name: %s", got)
	}
	if got := credentialFileName("", "sub/with:chars"); got != "xai-oauth-sub-with-chars.json" {
		t.Fatalf("unexpected sanitized name: %s", got)
	}
	if got := credentialFileName("", ""); got == "" {
		t.Fatalf("fallback name should not be empty")
	}
}

func TestHandleAuthParseAcceptsPluginFilesOnly(t *testing.T) {
	storage := buildTokenStorage(sampleToken(), "https://auth.x.ai/token")
	raw, err := json.Marshal(storage)
	if err != nil {
		t.Fatalf("marshal storage: %v", err)
	}
	request, err := json.Marshal(authParseRequest{FileName: "xai-oauth-user@example.com.json", RawJSON: raw})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	response, err := dispatch("auth.parse", request)
	if err != nil {
		t.Fatalf("dispatch auth.parse: %v", err)
	}
	var parsed authParseResponse
	if err := json.Unmarshal(envelopeResult(t, response), &parsed); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !parsed.Handled {
		t.Fatalf("plugin file should be handled")
	}
	if parsed.Auth.Label != "user@example.com" {
		t.Fatalf("unexpected label: %s", parsed.Auth.Label)
	}

	// Built-in xai files must not be claimed by the plugin.
	builtin := []byte(`{"type":"xai","access_token":"a","refresh_token":"r"}`)
	request, _ = json.Marshal(authParseRequest{FileName: "xai-user@example.com.json", RawJSON: builtin})
	response, err = dispatch("auth.parse", request)
	if err != nil {
		t.Fatalf("dispatch auth.parse builtin: %v", err)
	}
	parsed = authParseResponse{}
	if err := json.Unmarshal(envelopeResult(t, response), &parsed); err != nil {
		t.Fatalf("decode builtin response: %v", err)
	}
	if parsed.Handled {
		t.Fatalf("built-in xai files must not be handled by the plugin")
	}
}

func TestHandleAuthRefreshPreservesUserRouting(t *testing.T) {
	storage := buildTokenStorage(sampleToken(), "https://auth.x.ai/token")
	storage.UsingAPI = true
	storage.BaseURL = "https://proxy.example.com/v1"
	raw, err := json.Marshal(storage)
	if err != nil {
		t.Fatalf("marshal storage: %v", err)
	}
	server := httptestNewTokenServer(t, "access-2")
	defer server.Close()

	original := newOAuthClientFor
	newOAuthClientFor = func(host hostConfigSummary) *oauthClient {
		client := newOAuthClient(host.ProxyURL)
		client.httpClient = server.Client()
		client.validateEndpoint = acceptAnyEndpoint
		return client
	}
	defer func() { newOAuthClientFor = original }()

	storage.TokenEndpoint = server.URL + "/token"
	raw, err = json.Marshal(storage)
	if err != nil {
		t.Fatalf("re-marshal storage: %v", err)
	}
	request, _ := json.Marshal(authRefreshRequest{AuthID: "xai-oauth-user@example.com.json", StorageJSON: raw})
	response, err := dispatch("auth.refresh", request)
	if err != nil {
		t.Fatalf("dispatch auth.refresh: %v", err)
	}
	var refreshed authRefreshResponse
	if err := json.Unmarshal(envelopeResult(t, response), &refreshed); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var next tokenStorage
	if err := json.Unmarshal(refreshed.Auth.StorageJSON, &next); err != nil {
		t.Fatalf("decode storage: %v", err)
	}
	if next.AccessToken != "access-2" {
		t.Fatalf("expected refreshed token, got %s", next.AccessToken)
	}
	if next.BaseURL != "https://proxy.example.com/v1" {
		t.Fatalf("custom base URL should survive refresh, got %s", next.BaseURL)
	}
	if !next.usingAPI() {
		t.Fatalf("using_api should survive refresh")
	}
	if refreshed.Auth.Attributes["base_url"] != "https://proxy.example.com/v1" {
		t.Fatalf("attributes should carry custom base URL, got %s", refreshed.Auth.Attributes["base_url"])
	}
	if refreshed.Auth.Attributes["api_key"] != "access-2" {
		t.Fatalf("attributes should carry refreshed token, got %s", refreshed.Auth.Attributes["api_key"])
	}
}

func TestHandleLoginPollUnknownState(t *testing.T) {
	request, _ := json.Marshal(loginPollRequest{State: "missing"})
	response, err := dispatch("auth.login.poll", request)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	raw := response
	if !bytesContain(raw, `"ok":false`) {
		t.Fatalf("unknown state should return an error envelope: %s", raw)
	}
}
