package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// tokenStorage mirrors the built-in xai auth file shape so credentials stay
// recognizable. The host stamps "type" with the provider key on save.
type tokenStorage struct {
	Type          string `json:"type"`
	AccessToken   string `json:"access_token"`
	RefreshToken  string `json:"refresh_token"`
	IDToken       string `json:"id_token,omitempty"`
	TokenType     string `json:"token_type,omitempty"`
	ExpiresIn     int    `json:"expires_in,omitempty"`
	Expire        string `json:"expired,omitempty"`
	LastRefresh   string `json:"last_refresh,omitempty"`
	Email         string `json:"email,omitempty"`
	Subject       string `json:"sub,omitempty"`
	BaseURL       string `json:"base_url,omitempty"`
	RedirectURI   string `json:"redirect_uri,omitempty"`
	TokenEndpoint string `json:"token_endpoint,omitempty"`
	AuthKind      string `json:"auth_kind,omitempty"`
	UsingAPI      any    `json:"using_api,omitempty"`
	Prefix        string `json:"prefix,omitempty"`
}

// buildTokenStorage converts fresh token data into the persisted storage shape.
func buildTokenStorage(data *tokenData, tokenEndpoint string) *tokenStorage {
	if data == nil {
		return nil
	}
	return &tokenStorage{
		Type:          authProviderID,
		AccessToken:   data.AccessToken,
		RefreshToken:  data.RefreshToken,
		IDToken:       data.IDToken,
		TokenType:     data.TokenType,
		ExpiresIn:     data.ExpiresIn,
		Expire:        data.Expire,
		LastRefresh:   time.Now().UTC().Format(time.RFC3339),
		Email:         data.Email,
		Subject:       data.Subject,
		BaseURL:       defaultAPIBaseURL,
		TokenEndpoint: strings.TrimSpace(tokenEndpoint),
		AuthKind:      "oauth",
	}
}

func parseTokenStorage(raw []byte) (*tokenStorage, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, fmt.Errorf("xai-oauth storage: payload is empty")
	}
	var storage tokenStorage
	if err := json.Unmarshal([]byte(trimmed), &storage); err != nil {
		return nil, fmt.Errorf("xai-oauth storage: parse payload: %w", err)
	}
	return &storage, nil
}

// storageType reports the "type" field of a raw auth payload without full parsing.
func storageType(raw []byte) string {
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return ""
	}
	return strings.TrimSpace(header.Type)
}

// credentialFileName mirrors the built-in naming scheme with the plugin prefix.
func credentialFileName(email, subject string) string {
	if email = sanitizeFileSegment(email); email != "" {
		return fmt.Sprintf("%s-%s.json", pluginID, email)
	}
	if subject = sanitizeFileSegment(subject); subject != "" {
		return fmt.Sprintf("%s-%s.json", pluginID, subject)
	}
	return fmt.Sprintf("%s-%d.json", pluginID, time.Now().UnixMilli())
}

func sanitizeFileSegment(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '@' || r == '.' || r == '_' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// buildAuthData assembles the wire AuthData for a credential: storage JSON plus
// the attributes the host executor consumes for routing (base_url, custom
// headers) and model discovery.
//
// The credential fields the built-in xai executor's refresh chain reads live in
// Metadata, not StorageJSON: since the auth provider identity is "xai", the host
// binds the native XAI(Auto)Executor to this auth, and its Refresh resolves
// refresh_token/token_endpoint from Metadata (xai_executor_auth.go). Mirroring
// access_token/refresh_token/expired/last_refresh here is what enables
// scheduled refresh and the 401 refresh-retry; without them every refresh is a
// silent no-op and the token expires in place.
//
// Attributes deliberately does NOT carry api_key: xaiCreds() prefers the stale
// attribute over the metadata access_token, and the attribute is only rebuilt on
// parse, so keeping it would send expired bearer tokens after a refresh.
func buildAuthData(storage *tokenStorage, fileName string) (*authData, error) {
	if storage == nil {
		return nil, fmt.Errorf("xai-oauth: storage is nil")
	}
	raw, err := json.Marshal(storage)
	if err != nil {
		return nil, fmt.Errorf("xai-oauth: encode storage: %w", err)
	}
	label := strings.TrimSpace(storage.Email)
	if label == "" {
		label = strings.TrimSpace(storage.Subject)
	}
	usingAPI := storage.usingAPI()
	baseURL := authBaseURL(storage, usingAPI)
	prefix := strings.TrimSpace(storage.Prefix)
	if prefix == "" {
		prefix = pluginPrefix
	}
	attributes := map[string]string{
		"base_url":                         baseURL,
		"auth_kind":                        "oauth",
		"using_api":                        strconv.FormatBool(usingAPI),
		"header:" + tokenAuthHeader:        tokenAuthValue,
		"header:" + clientVersionHeader:    grokClientVersion,
		"header:" + clientIdentifierHeader: clientIdentifierValue,
		"header:User-Agent":                grokWorkspaceUserAgent,
	}
	metadata := map[string]any{
		"auth_kind": "oauth",
	}
	if storage.AccessToken != "" {
		metadata["access_token"] = storage.AccessToken
	}
	if storage.RefreshToken != "" {
		metadata["refresh_token"] = storage.RefreshToken
	}
	if storage.Expire != "" {
		metadata["expired"] = storage.Expire
	}
	if storage.LastRefresh != "" {
		metadata["last_refresh"] = storage.LastRefresh
	}
	if storage.Email != "" {
		metadata["email"] = storage.Email
	}
	if storage.Subject != "" {
		metadata["sub"] = storage.Subject
	}
	if storage.BaseURL != "" {
		metadata["base_url"] = storage.BaseURL
	}
	if storage.TokenEndpoint != "" {
		metadata["token_endpoint"] = storage.TokenEndpoint
	}
	if storage.UsingAPI != nil {
		metadata["using_api"] = usingAPI
	}
	if prefix != "" {
		metadata["prefix"] = prefix
	}
	return &authData{
		Provider:         authProviderID,
		ID:               fileName,
		FileName:         fileName,
		Label:            label,
		Prefix:           prefix,
		StorageJSON:      raw,
		Metadata:         metadata,
		Attributes:       attributes,
		NextRefreshAfter: formatNextRefreshAfter(storage.Expire, storage.ExpiresIn),
	}, nil
}

// usingAPI reports whether the credential routes to the official api.x.ai base.
// Default false: OAuth credentials use the CLI chat proxy.
func (s *tokenStorage) usingAPI() bool {
	if s == nil {
		return false
	}
	switch value := s.UsingAPI.(type) {
	case bool:
		return value
	case string:
		if parsed, err := strconv.ParseBool(strings.TrimSpace(value)); err == nil {
			return parsed
		}
	}
	return false
}

// authBaseURL resolves the routing base URL: an explicit custom base_url wins,
// otherwise OAuth defaults to the CLI chat proxy and using_api to api.x.ai.
func authBaseURL(storage *tokenStorage, usingAPI bool) string {
	if storage == nil {
		return defaultAPIBaseURL
	}
	explicit := strings.TrimRight(strings.TrimSpace(storage.BaseURL), "/")
	if explicit != "" &&
		!strings.EqualFold(explicit, defaultAPIBaseURL) &&
		!strings.EqualFold(explicit, cliChatProxyBaseURL) {
		return explicit
	}
	if usingAPI {
		return defaultAPIBaseURL
	}
	return cliChatProxyBaseURL
}

// formatNextRefreshAfter returns the RFC3339 time at which the host should next
// refresh the credential. The absolute expiry timestamp wins: parsing an
// already-expired or partially-consumed credential must schedule the refresh
// relative to real expiry, not "now + expires_in" (which would defer a stale
// token by a full lifetime every time the host re-parses its file). Only when
// no usable timestamp exists does it fall back to the relative window, clamped
// so the delay never exceeds expires_in.
func formatNextRefreshAfter(expiresAt string, expiresIn int) string {
	now := time.Now().UTC()
	if expiry, err := time.Parse(time.RFC3339, strings.TrimSpace(expiresAt)); err == nil && !expiry.IsZero() {
		due := expiry.Add(-refreshLead)
		if expiresIn > 0 {
			if earliest := now.Add(time.Duration(expiresIn) * time.Second); earliest.Before(due) {
				due = earliest
			}
		}
		if due.Before(now.Add(15 * time.Second)) {
			due = now.Add(15 * time.Second)
		}
		return due.Format(time.RFC3339)
	}
	delay := time.Duration(expiresIn)*time.Second - refreshLead
	if delay < time.Minute {
		delay = time.Duration(expiresIn/2) * time.Second
	}
	if delay < 15*time.Second {
		delay = 15 * time.Second
	}
	return now.Add(delay).Format(time.RFC3339)
}

// metadataString reads a string value from a host-provided metadata map.
func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	if value, ok := metadata[key].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

// metadataBool reads a bool value (JSON round trips make numbers float64) from metadata.
func metadataBool(metadata map[string]any, key string) (value, ok bool) {
	if metadata == nil {
		return false, false
	}
	switch raw := metadata[key].(type) {
	case bool:
		return raw, true
	case string:
		if parsed, err := strconv.ParseBool(strings.TrimSpace(raw)); err == nil {
			return parsed, true
		}
	}
	return false, false
}
