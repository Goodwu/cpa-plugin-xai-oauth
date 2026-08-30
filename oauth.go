package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// xAI Grok CLI OAuth constants (kept in sync with the built-in internal/auth/xai package).
const (
	defaultAPIBaseURL     = "https://api.x.ai/v1"
	cliChatProxyBaseURL   = "https://cli-chat-proxy.grok.com/v1"
	oauthIssuer           = "https://auth.x.ai"
	discoveryURL          = oauthIssuer + "/.well-known/openid-configuration"
	oauthClientID         = "b1a00492-073a-47ea-816f-4c329264a828"
	oauthScope            = "openid profile email offline_access grok-cli:access api:access"
	deviceCodeGrantType   = "urn:ietf:params:oauth:grant-type:device_code"
	credentialHTTPTimeout = 30 * time.Second
	discoveryCacheTTL     = time.Hour
	refreshLead           = 5 * time.Minute
	// grokClientVersion must match the Grok CLI client version chat-proxy expects.
	grokClientVersion      = "0.2.120"
	tokenAuthHeader        = "X-XAI-Token-Auth"
	tokenAuthValue         = "xai-grok-cli"
	clientVersionHeader    = "x-grok-client-version"
	clientIdentifierHeader = "x-grok-client-identifier"
	clientIdentifierValue  = "grok-shell"
	authenticateRespHeader = "x-authenticateresponse"
	authenticateRespValue  = "authenticate-response"
	grokWorkspaceUserAgent = "xai-grok-workspace/" + grokClientVersion
)

// defaultPollInterval is the RFC 8628 polling floor when the device endpoint
// omits interval. It is a var so tests can collapse the wait.
var defaultPollInterval = 5 * time.Second

type discovery struct {
	DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
	TokenEndpoint               string `json:"token_endpoint"`
}

type tokenData struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
	Expire       string `json:"expired,omitempty"`
	Email        string `json:"email,omitempty"`
	Subject      string `json:"sub,omitempty"`
}

type deviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
	TokenEndpoint           string `json:"-"`
}

// endpointValidator checks a discovered OAuth endpoint. It is an indirection
// point so tests can accept loopback URLs; production always uses fail-closed
// validateOAuthEndpoint.
type endpointValidator func(rawURL, field string) (string, error)

// oauthClient performs xAI OIDC discovery, device-code exchange, and refresh.
type oauthClient struct {
	mu         sync.Mutex
	httpClient *http.Client
	proxyURL   string
	cached     *discovery
	cachedAt   time.Time
	// discoveryOverride replaces the OIDC discovery URL in tests.
	discoveryOverride string
	validateEndpoint  endpointValidator
}

func newOAuthClient(proxyURL string) *oauthClient {
	proxyURL = strings.TrimSpace(proxyURL)
	client := &http.Client{Timeout: credentialHTTPTimeout}
	if transport := proxyTransport(proxyURL); transport != nil {
		client.Transport = transport
	}
	return &oauthClient{httpClient: client, proxyURL: proxyURL, validateEndpoint: validateOAuthEndpoint}
}

// modelsClient returns an unbounded HTTP client for model discovery calls.
// The lifetime is bounded by the request context, mirroring the built-in executor.
func (o *oauthClient) modelsClient() *http.Client {
	if o == nil {
		return &http.Client{}
	}
	client := &http.Client{}
	if o.httpClient != nil && o.httpClient.Transport != nil {
		client.Transport = o.httpClient.Transport
	}
	return client
}

func proxyTransport(proxyURL string) http.RoundTripper {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return nil
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil
	}
	return &http.Transport{Proxy: http.ProxyURL(parsed)}
}

// discover resolves OAuth endpoints via OIDC discovery with a small cache.
func (o *oauthClient) discover(ctx context.Context) (*discovery, error) {
	if o == nil {
		return nil, fmt.Errorf("xai oauth: client is nil")
	}
	o.mu.Lock()
	if o.cached != nil && time.Since(o.cachedAt) < discoveryCacheTTL {
		cached := o.cached
		o.mu.Unlock()
		return cached, nil
	}
	o.mu.Unlock()

	discoveryEndpoint := discoveryURL
	if o.discoveryOverride != "" {
		discoveryEndpoint = o.discoveryOverride
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryEndpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("xai discovery: create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("xai discovery: request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("xai discovery: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("xai discovery failed with status %d: %s", resp.StatusCode, truncateForLog(body))
	}
	var payload discovery
	if err = json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("xai discovery: parse response: %w", err)
	}
	validate := o.validateEndpoint
	if validate == nil {
		validate = validateOAuthEndpoint
	}
	deviceEndpoint, err := validate(payload.DeviceAuthorizationEndpoint, "device_authorization_endpoint")
	if err != nil {
		return nil, err
	}
	tokenEndpoint, err := validate(payload.TokenEndpoint, "token_endpoint")
	if err != nil {
		return nil, err
	}
	resolved := &discovery{DeviceAuthorizationEndpoint: deviceEndpoint, TokenEndpoint: tokenEndpoint}
	o.mu.Lock()
	o.cached = resolved
	o.cachedAt = time.Now()
	o.mu.Unlock()
	return resolved, nil
}

// validateOAuthEndpoint fails closed: only https endpoints on x.ai are accepted.
func validateOAuthEndpoint(rawURL string, field string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", fmt.Errorf("xai discovery %s is empty", field)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("xai discovery %s is invalid: %w", field, err)
	}
	if parsed.Scheme != "https" {
		return "", fmt.Errorf("xai discovery %s must use https: %q", field, rawURL)
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	if host != "x.ai" && !strings.HasSuffix(host, ".x.ai") {
		return "", fmt.Errorf("xai discovery %s host %q is not on x.ai", field, host)
	}
	return rawURL, nil
}

func (o *oauthClient) requestDeviceCode(ctx context.Context) (*deviceCodeResponse, error) {
	endpoints, err := o.discover(ctx)
	if err != nil {
		return nil, err
	}
	form := url.Values{
		"client_id": {oauthClientID},
		"scope":     {oauthScope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoints.DeviceAuthorizationEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("xai device code: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("xai device code request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("xai device code: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("xai device code request failed with status %d: %s", resp.StatusCode, truncateForLog(body))
	}
	var deviceCode deviceCodeResponse
	if err = json.Unmarshal(body, &deviceCode); err != nil {
		return nil, fmt.Errorf("xai device code: parse response: %w", err)
	}
	if strings.TrimSpace(deviceCode.DeviceCode) == "" {
		return nil, fmt.Errorf("xai device code: response missing device_code")
	}
	if strings.TrimSpace(deviceCode.UserCode) == "" {
		return nil, fmt.Errorf("xai device code: response missing user_code")
	}
	if strings.TrimSpace(deviceCode.VerificationURI) == "" && strings.TrimSpace(deviceCode.VerificationURIComplete) == "" {
		return nil, fmt.Errorf("xai device code: response missing verification URI")
	}
	deviceCode.TokenEndpoint = strings.TrimSpace(endpoints.TokenEndpoint)
	return &deviceCode, nil
}

// exchangeOutcome describes one device-code token exchange attempt.
type exchangeOutcome struct {
	Status       string // "success" | "pending" | "slow_down" | "error"
	Message      string
	Token        *tokenData
	NextInterval time.Duration
}

// exchangeDeviceCode performs one RFC 8628 token exchange attempt.
func (o *oauthClient) exchangeDeviceCode(ctx context.Context, tokenEndpoint, deviceCode string, interval time.Duration) exchangeOutcome {
	if interval <= 0 {
		interval = defaultPollInterval
	}
	form := url.Values{
		"grant_type":  {deviceCodeGrantType},
		"device_code": {strings.TrimSpace(deviceCode)},
		"client_id":   {oauthClientID},
	}
	payload, err := o.postTokenForm(ctx, tokenEndpoint, form)
	if err != nil {
		return exchangeOutcome{Status: "error", Message: err.Error()}
	}
	if payload.oauthError != "" {
		switch payload.oauthError {
		case "authorization_pending":
			return exchangeOutcome{Status: "pending", NextInterval: interval}
		case "slow_down":
			return exchangeOutcome{Status: "slow_down", NextInterval: interval + defaultPollInterval}
		case "expired_token":
			return exchangeOutcome{Status: "error", Message: "xai device code expired"}
		case "access_denied":
			return exchangeOutcome{Status: "error", Message: "xai device authorization denied"}
		default:
			message := fmt.Sprintf("xai device token error: %s", payload.oauthError)
			if payload.oauthErrorDescription != "" {
				message = fmt.Sprintf("xai device token error: %s: %s", payload.oauthError, payload.oauthErrorDescription)
			}
			return exchangeOutcome{Status: "error", Message: message}
		}
	}
	return exchangeOutcome{Status: "success", Token: payload.token}
}

// refreshTokens exchanges a refresh token for fresh token data.
func (o *oauthClient) refreshTokens(ctx context.Context, refreshToken, tokenEndpoint string) (*tokenData, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil, fmt.Errorf("xai token refresh: refresh token is required")
	}
	if strings.TrimSpace(tokenEndpoint) == "" {
		endpoints, err := o.discover(ctx)
		if err != nil {
			return nil, err
		}
		tokenEndpoint = endpoints.TokenEndpoint
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {oauthClientID},
		"refresh_token": {refreshToken},
	}
	payload, err := o.postTokenForm(ctx, tokenEndpoint, form)
	if err != nil {
		return nil, err
	}
	if payload == nil || payload.token == nil {
		return nil, fmt.Errorf("xai token refresh: empty response")
	}
	return payload.token, nil
}

type tokenFormPayload struct {
	token                 *tokenData
	oauthError            string
	oauthErrorDescription string
}

func (o *oauthClient) postTokenForm(ctx context.Context, tokenEndpoint string, form url.Values) (*tokenFormPayload, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSpace(tokenEndpoint), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("xai token request: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("xai token request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("xai token response: read body: %w", err)
	}
	var payload struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		IDToken          string `json:"id_token"`
		TokenType        string `json:"token_type"`
		ExpiresIn        int    `json:"expires_in"`
	}
	if err = json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("xai token response: parse body: %w", err)
	}
	if payload.Error != "" {
		return &tokenFormPayload{oauthError: payload.Error, oauthErrorDescription: payload.ErrorDescription}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("xai token request failed with status %d: %s", resp.StatusCode, truncateForLog(body))
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return nil, fmt.Errorf("xai token response missing access_token")
	}
	email, subject := parseJWTIdentity(payload.IDToken)
	return &tokenFormPayload{token: buildTokenData(payload.AccessToken, payload.RefreshToken, payload.IDToken, payload.TokenType, payload.ExpiresIn, email, subject)}, nil
}

func buildTokenData(accessToken, refreshToken, idToken, tokenType string, expiresIn int, email, subject string) *tokenData {
	data := &tokenData{
		AccessToken:  strings.TrimSpace(accessToken),
		RefreshToken: strings.TrimSpace(refreshToken),
		IDToken:      strings.TrimSpace(idToken),
		TokenType:    strings.TrimSpace(tokenType),
		ExpiresIn:    expiresIn,
		Email:        email,
		Subject:      subject,
	}
	if expiresIn > 0 {
		data.Expire = time.Now().Add(time.Duration(expiresIn) * time.Second).UTC().Format(time.RFC3339)
	}
	return data
}

func parseJWTIdentity(token string) (email string, subject string) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", ""
	}
	payload := parts[1]
	payload += strings.Repeat("=", (4-len(payload)%4)%4)
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return "", ""
	}
	var claims map[string]any
	if err = json.Unmarshal(raw, &claims); err != nil {
		return "", ""
	}
	if v, ok := claims["email"].(string); ok {
		email = strings.TrimSpace(v)
	}
	if v, ok := claims["sub"].(string); ok {
		subject = strings.TrimSpace(v)
	}
	return email, subject
}

func truncateForLog(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 300 {
		return text[:300]
	}
	return text
}
