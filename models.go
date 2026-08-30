package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const modelsMaxResponseBytes = int64(4 << 20)

// wire request payloads.
type staticModelRequest struct {
	Plugin pluginMetadata    `json:"Plugin"`
	Host   hostConfigSummary `json:"Host"`
}

type authModelRequest struct {
	Plugin       pluginMetadata    `json:"Plugin"`
	AuthID       string            `json:"AuthID"`
	AuthProvider string            `json:"AuthProvider"`
	StorageJSON  []byte            `json:"StorageJSON"`
	Metadata     map[string]any    `json:"Metadata"`
	Attributes   map[string]string `json:"Attributes"`
	Host         hostConfigSummary `json:"Host"`
}

type modelResponse struct {
	Provider   string          `json:"Provider"`
	Models     []wireModelInfo `json:"Models"`
	AuthUpdate authData        `json:"AuthUpdate"`
}

// remoteModel mirrors the Grok chat-proxy /models response item.
type remoteModel struct {
	ID                  string         `json:"id"`
	Model               string         `json:"model"`
	Object              string         `json:"object"`
	Created             int64          `json:"created"`
	OwnedBy             string         `json:"owned_by"`
	Name                string         `json:"name"`
	Description         string         `json:"description"`
	ContextWindow       int            `json:"context_window"`
	ContextLength       int            `json:"context_length"`
	MaxCompletionTokens int            `json:"max_completion_tokens"`
	SupportsReasoning   *bool          `json:"supports_reasoning_effort"`
	ReasoningEffort     string         `json:"reasoning_effort"`
	ReasoningEfforts    []remoteEffort `json:"reasoning_efforts"`
}

type remoteEffort struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

type modelsPayload struct {
	Data   *[]remoteModel `json:"data"`
	Models *[]remoteModel `json:"models"`
}

func handleStaticModels(request []byte) ([]byte, error) {
	return okEnvelope(modelResponse{Provider: pluginID, Models: catalogModels()})
}

func handleModelsForAuth(request []byte) ([]byte, error) {
	var req authModelRequest
	if err := decodeRequest(request, &req); err != nil {
		return errorEnvelope("bad_request", err.Error()), nil
	}
	storage, _ := parseTokenStorage(req.StorageJSON)
	token := credentialToken(req, storage)
	if token == "" {
		return httpErrorEnvelope(http.StatusUnauthorized, "xai-oauth models: access token is missing"), nil
	}
	baseURL, cli := resolveModelsRoute(req, storage)
	models, err := fetchModels(context.Background(), req.Host, token, baseURL, cli, req.Attributes)
	if err != nil {
		return httpErrorEnvelope(http.StatusBadGateway, err.Error()), nil
	}
	return okEnvelope(modelResponse{Provider: pluginID, Models: models})
}

// credentialToken resolves the bearer token from attributes, metadata, then storage.
func credentialToken(req authModelRequest, storage *tokenStorage) string {
	for _, key := range []string{"api_key", "access_token"} {
		if value := strings.TrimSpace(req.Attributes[key]); value != "" {
			return value
		}
	}
	for _, key := range []string{"access_token", "api_key"} {
		if value := metadataString(req.Metadata, key); value != "" {
			return value
		}
	}
	if storage != nil {
		if value := strings.TrimSpace(storage.AccessToken); value != "" {
			return value
		}
	}
	return ""
}

// resolveModelsRoute mirrors the built-in xai routing: an explicit custom
// base_url wins, OAuth defaults to the CLI chat proxy with CLI identity
// headers, and using_api switches to api.x.ai.
func resolveModelsRoute(req authModelRequest, storage *tokenStorage) (string, bool) {
	usingAPI := usingAPIForModels(req, storage)
	explicit := authString(req, storage, "base_url")
	if explicit != "" &&
		!strings.EqualFold(explicit, defaultAPIBaseURL) &&
		!strings.EqualFold(explicit, cliChatProxyBaseURL) {
		return explicit, false
	}
	if usingAPI {
		if explicit == "" || strings.EqualFold(explicit, cliChatProxyBaseURL) {
			return defaultAPIBaseURL, false
		}
		return explicit, false
	}
	return cliChatProxyBaseURL, true
}

// authString reads a routing value from attributes, metadata, then storage.
func authString(req authModelRequest, storage *tokenStorage, key string) string {
	if value := strings.TrimSpace(req.Attributes[key]); value != "" {
		return value
	}
	if value := metadataString(req.Metadata, key); value != "" {
		return value
	}
	if storage != nil {
		switch key {
		case "base_url":
			return strings.TrimRight(strings.TrimSpace(storage.BaseURL), "/")
		}
	}
	return ""
}

func usingAPIForModels(req authModelRequest, storage *tokenStorage) bool {
	if raw := strings.TrimSpace(req.Attributes["using_api"]); raw != "" {
		if parsed, err := strconv.ParseBool(raw); err == nil {
			return parsed
		}
	}
	if value, ok := metadataBool(req.Metadata, "using_api"); ok {
		return value
	}
	if storage != nil && storage.UsingAPI != nil {
		return storage.usingAPI()
	}
	authKind := strings.ToLower(strings.TrimSpace(req.Attributes["auth_kind"]))
	if authKind == "" {
		authKind = strings.ToLower(metadataString(req.Metadata, "auth_kind"))
	}
	if authKind == "" && storage != nil {
		authKind = strings.ToLower(strings.TrimSpace(storage.AuthKind))
	}
	return authKind != "oauth"
}

func fetchModels(ctx context.Context, host hostConfigSummary, token, baseURL string, cli bool, attributes map[string]string) ([]wireModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("xai models: create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if cli {
		req.Header.Set(tokenAuthHeader, tokenAuthValue)
		req.Header.Set(clientVersionHeader, grokClientVersion)
		req.Header.Set("User-Agent", grokWorkspaceUserAgent)
		req.Header.Set(clientIdentifierHeader, clientIdentifierValue)
		req.Header.Set(authenticateRespHeader, authenticateRespValue)
	}
	for key, value := range attributes {
		if !strings.HasPrefix(key, "header:") {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(key, "header:"))
		if name == "" || strings.TrimSpace(value) == "" {
			continue
		}
		req.Header.Set(name, value)
	}
	client := newOAuthClientFor(host).modelsClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("xai models: request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, modelsMaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("xai models: read response: %w", err)
	}
	if int64(len(body)) > modelsMaxResponseBytes {
		return nil, fmt.Errorf("xai models: response exceeds %d bytes", modelsMaxResponseBytes)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("xai models: upstream returned status %d: %s", resp.StatusCode, truncateForLog(body))
	}
	var payload modelsPayload
	if err = json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("xai models: parse response: %w", err)
	}
	var remote []remoteModel
	switch {
	case payload.Data != nil:
		remote = *payload.Data
	case payload.Models != nil:
		remote = *payload.Models
	default:
		return nil, fmt.Errorf("xai models: response missing data/models field")
	}
	return mergeModels(remote, catalogModels()), nil
}

// mergeModels keeps remote discovery authoritative and backfills metadata
// from the embedded static catalog, mirroring the built-in merge behavior.
func mergeModels(remote []remoteModel, catalog []wireModelInfo) []wireModelInfo {
	byID := make(map[string]wireModelInfo, len(catalog))
	for _, model := range catalog {
		if model.ID != "" {
			byID[strings.ToLower(model.ID)] = model
		}
	}
	result := make([]wireModelInfo, 0, len(remote))
	seen := make(map[string]struct{}, len(remote))
	for _, item := range remote {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			id = strings.TrimSpace(item.Model)
		}
		key := strings.ToLower(id)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		model := wireModelInfo{
			ID:                  id,
			Object:              item.Object,
			Created:             item.Created,
			OwnedBy:             item.OwnedBy,
			Type:                "xai",
			DisplayName:         item.Name,
			Name:                item.Name,
			Description:         item.Description,
			ContextLength:       int64(item.ContextWindow),
			MaxCompletionTokens: int64(item.MaxCompletionTokens),
		}
		if model.Object == "" {
			model.Object = "model"
		}
		if model.OwnedBy == "" {
			model.OwnedBy = "xAI"
		}
		if model.ContextLength == 0 {
			model.ContextLength = int64(item.ContextLength)
		}
		if static, ok := byID[key]; ok {
			model = mergeModelMetadata(static, model)
		}
		if model.DisplayName == "" {
			model.DisplayName = id
		}
		if model.Name == "" {
			model.Name = id
		}
		levels := make([]string, 0, len(item.ReasoningEfforts))
		for _, effort := range item.ReasoningEfforts {
			value := strings.TrimSpace(effort.Value)
			if value == "" {
				value = strings.TrimSpace(effort.ID)
			}
			if value != "" {
				levels = append(levels, value)
			}
		}
		if len(levels) == 0 && strings.TrimSpace(item.ReasoningEffort) != "" {
			levels = []string{strings.TrimSpace(item.ReasoningEffort)}
		}
		if len(levels) > 0 {
			model.Thinking = &wireThinkingSupport{Levels: levels}
		}
		if item.SupportsReasoning != nil && !*item.SupportsReasoning {
			model.Thinking = nil
		}
		result = append(result, model)
	}
	return result
}

func mergeModelMetadata(static, remote wireModelInfo) wireModelInfo {
	merged := static
	if static.Thinking != nil {
		thinking := *static.Thinking
		thinking.Levels = append([]string(nil), static.Thinking.Levels...)
		merged.Thinking = &thinking
	}
	if remote.ID != "" {
		merged.ID = remote.ID
	}
	if remote.Object != "" {
		merged.Object = remote.Object
	}
	if remote.Created != 0 {
		merged.Created = remote.Created
	}
	if remote.OwnedBy != "" {
		merged.OwnedBy = remote.OwnedBy
	}
	if remote.Type != "" {
		merged.Type = remote.Type
	}
	if remote.DisplayName != "" {
		merged.DisplayName = remote.DisplayName
	}
	if remote.Name != "" {
		merged.Name = remote.Name
	}
	if remote.Description != "" {
		merged.Description = remote.Description
	}
	if remote.ContextLength != 0 {
		merged.ContextLength = remote.ContextLength
	}
	if remote.MaxCompletionTokens != 0 {
		merged.MaxCompletionTokens = remote.MaxCompletionTokens
	}
	return merged
}
