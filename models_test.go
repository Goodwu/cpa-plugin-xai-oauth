package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMergeModelsBackfillsCatalogMetadata(t *testing.T) {
	falseValue := false
	remote := []remoteModel{
		{ID: "grok-4.6", Object: "model", Created: 123, OwnedBy: "xAI", ReasoningEfforts: []remoteEffort{{ID: "low", Value: "low"}, {ID: "high", Value: "high"}}},
		{ID: "brand-new-model", Name: "Brand New", ContextWindow: 131072},
		{Model: "alias-model", SupportsReasoning: &falseValue, ReasoningEffort: "low"},
		{ID: "grok-4.6"},
	}
	merged := mergeModels(remote, catalogModels())
	if len(merged) != 3 {
		t.Fatalf("expected 3 unique models, got %d", len(merged))
	}
	first := merged[0]
	if first.ID != "grok-4.6" || first.OwnedBy != "xAI" {
		t.Fatalf("unexpected first model: %+v", first)
	}
	if first.ContextLength != 500000 {
		t.Fatalf("catalog context length should be backfilled, got %d", first.ContextLength)
	}
	if first.Description == "" {
		t.Fatalf("catalog description should be backfilled")
	}
	if first.Thinking == nil || len(first.Thinking.Levels) != 2 {
		t.Fatalf("remote reasoning efforts should map to thinking levels: %+v", first.Thinking)
	}
	second := merged[1]
	if second.OwnedBy != "xAI" || second.Object != "model" || second.ContextLength != 131072 {
		t.Fatalf("unknown model should keep remote values with defaults: %+v", second)
	}
	third := merged[2]
	if third.ID != "alias-model" || third.Thinking != nil {
		t.Fatalf("supports_reasoning_effort=false should drop thinking: %+v", third)
	}
}

func TestResolveModelsRoute(t *testing.T) {
	// OAuth default: chat proxy with CLI identity headers.
	storage := buildTokenStorage(sampleToken(), "https://auth.x.ai/token")
	data, err := buildAuthData(storage, "xai-oauth-user@example.com.json")
	if err != nil {
		t.Fatalf("buildAuthData: %v", err)
	}
	req := authModelRequest{Attributes: data.Attributes, Metadata: data.Metadata}
	base, cli := resolveModelsRoute(req, storage)
	if base != cliChatProxyBaseURL || !cli {
		t.Fatalf("OAuth default should be chat proxy with CLI headers, got %s cli=%v", base, cli)
	}

	// using_api=true routes to api.x.ai without CLI headers (metadata carries base_url).
	storage = buildTokenStorage(sampleToken(), "https://auth.x.ai/token")
	storage.UsingAPI = true
	data, _ = buildAuthData(storage, "xai-oauth-user@example.com.json")
	req = authModelRequest{Attributes: data.Attributes, Metadata: data.Metadata}
	base, cli = resolveModelsRoute(req, storage)
	if base != defaultAPIBaseURL || cli {
		t.Fatalf("using_api should route to api.x.ai without CLI headers, got %s cli=%v", base, cli)
	}

	// Custom gateway base_url wins over both defaults.
	storage = buildTokenStorage(sampleToken(), "https://auth.x.ai/token")
	storage.BaseURL = "https://gateway.example.com/v1"
	data, _ = buildAuthData(storage, "xai-oauth-user@example.com.json")
	req = authModelRequest{Attributes: data.Attributes, Metadata: data.Metadata}
	base, cli = resolveModelsRoute(req, storage)
	if base != "https://gateway.example.com/v1" || cli {
		t.Fatalf("custom base_url should win without CLI headers, got %s cli=%v", base, cli)
	}
}

func TestHandleModelsForAuthRequiresToken(t *testing.T) {
	request, _ := json.Marshal(authModelRequest{Attributes: map[string]string{}})
	response, err := dispatch("model.for_auth", request)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !bytesContain(response, `"ok":false`) {
		t.Fatalf("missing token should error: %s", response)
	}
}

func TestHandleModelsForAuthUpstream(t *testing.T) {
	var gotAuth, gotTokenAuth, gotVersion, gotUA string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		gotTokenAuth = r.Header.Get(tokenAuthHeader)
		gotVersion = r.Header.Get(clientVersionHeader)
		gotUA = r.Header.Get("User-Agent")
		writeJSON(t, w, http.StatusOK, modelsPayload{Data: &[]remoteModel{
			{ID: "grok-4.6", Object: "model", OwnedBy: "xAI", ReasoningEfforts: []remoteEffort{{Value: "low"}}},
		}})
	}))
	defer server.Close()

	// Real credentials carry CLI identity via header:* attributes (from buildAuthData).
	data, err := buildAuthData(buildTokenStorage(sampleToken(), "https://auth.x.ai/token"), "xai-oauth-user@example.com.json")
	if err != nil {
		t.Fatalf("buildAuthData: %v", err)
	}
	attributes := make(map[string]string, len(data.Attributes))
	for key, value := range data.Attributes {
		attributes[key] = value
	}
	attributes["base_url"] = server.URL // route discovery through the loopback server
	request, _ := json.Marshal(authModelRequest{Attributes: attributes})
	original := newOAuthClientFor
	newOAuthClientFor = func(hostConfigSummary) *oauthClient {
		client := newOAuthClient("")
		client.httpClient = server.Client()
		client.discoveryOverride = server.URL + "/.well-known/openid-configuration"
		return client
	}
	defer func() { newOAuthClientFor = original }()

	response, err := dispatch("model.for_auth", request)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if gotAuth != "Bearer access-1" {
		t.Fatalf("unexpected authorization header: %s", gotAuth)
	}
	if gotTokenAuth != tokenAuthValue || gotVersion != grokClientVersion || gotUA != grokWorkspaceUserAgent {
		t.Fatalf("missing CLI identity headers: %s %s %s", gotTokenAuth, gotVersion, gotUA)
	}
	var models modelResponse
	if err := json.Unmarshal(envelopeResult(t, response), &models); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if models.Provider != pluginID || len(models.Models) != 1 {
		t.Fatalf("unexpected models response: %+v", models)
	}
	if models.Models[0].ID != "grok-4.6" || models.Models[0].Thinking == nil {
		t.Fatalf("unexpected model mapping: %+v", models.Models[0])
	}
}

func TestCatalogLoads(t *testing.T) {
	models := catalogModels()
	if len(models) == 0 {
		t.Fatalf("embedded catalog should not be empty")
	}
	for _, model := range models {
		if model.ID == "" || model.Object == "" {
			t.Fatalf("catalog entries must carry id and object: %+v", model)
		}
	}
}

func TestDispatchRegistrationAndIdentifier(t *testing.T) {
	response, err := dispatch("plugin.register", nil)
	if err != nil {
		t.Fatalf("dispatch register: %v", err)
	}
	var registration registrationResponse
	if err := json.Unmarshal(envelopeResult(t, response), &registration); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	if registration.SchemaVersion != schemaVersion || !registration.Capabilities.AuthProvider || !registration.Capabilities.ModelProvider {
		t.Fatalf("unexpected registration: %+v", registration)
	}
	// The host's validPlugin() rejects registrations missing any of these.
	if registration.Metadata.Name != pluginID ||
		registration.Metadata.Version == "" ||
		registration.Metadata.Author == "" ||
		registration.Metadata.GitHubRepository == "" {
		t.Fatalf("registration metadata incomplete: %+v", registration.Metadata)
	}

	response, err = dispatch("auth.identifier", nil)
	if err != nil {
		t.Fatalf("dispatch identifier: %v", err)
	}
	result := envelopeResult(t, response)
	if !bytesContain(result, authProviderID) {
		t.Fatalf("identifier should be %s: %s", authProviderID, result)
	}
}
