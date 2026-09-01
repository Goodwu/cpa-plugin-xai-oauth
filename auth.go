package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// loginSession tracks one in-flight device flow. State arrives from the
// StartLogin metadata round trip and pacing adjustments live in this map so
// slow_down bookkeeping survives between polls.
type loginSession struct {
	DeviceCode    string
	TokenEndpoint string
	Interval      time.Duration
	ExpiresAt     time.Time
	NextAttempt   time.Time
}

type loginSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*loginSession
}

var sessions = &loginSessionStore{sessions: make(map[string]*loginSession)}

func (s *loginSessionStore) put(state string, session *loginSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	s.sessions[state] = session
}

func (s *loginSessionStore) get(state string) (*loginSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[state]
	return session, ok
}

func (s *loginSessionStore) delete(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, state)
}

func (s *loginSessionStore) pruneLocked() {
	now := time.Now()
	for state, session := range s.sessions {
		if now.After(session.ExpiresAt.Add(10 * time.Minute)) {
			delete(s.sessions, state)
		}
	}
}

// wire request payloads (mirror the pluginapi structs field-for-field).
type authParseRequest struct {
	Provider string            `json:"Provider"`
	Path     string            `json:"Path"`
	FileName string            `json:"FileName"`
	RawJSON  []byte            `json:"RawJSON"`
	Host     hostConfigSummary `json:"Host"`
}

type authParseResponse struct {
	Handled bool       `json:"Handled"`
	Auth    authData   `json:"Auth"`
	Auths   []authData `json:"Auths,omitempty"`
}

type loginStartRequest struct {
	Provider string            `json:"Provider"`
	BaseURL  string            `json:"BaseURL"`
	Host     hostConfigSummary `json:"Host"`
	Metadata map[string]any    `json:"Metadata"`
}

type loginStartResponse struct {
	Provider  string         `json:"Provider"`
	URL       string         `json:"URL"`
	State     string         `json:"State"`
	ExpiresAt time.Time      `json:"ExpiresAt"`
	Metadata  map[string]any `json:"Metadata,omitempty"`
}

type loginPollRequest struct {
	Provider string            `json:"Provider"`
	State    string            `json:"State"`
	Host     hostConfigSummary `json:"Host"`
	Metadata map[string]any    `json:"Metadata"`
}

type loginPollResponse struct {
	Status  string     `json:"Status"`
	Message string     `json:"Message,omitempty"`
	Auth    authData   `json:"Auth"`
	Auths   []authData `json:"Auths,omitempty"`
}

type authRefreshRequest struct {
	AuthID       string            `json:"AuthID"`
	AuthProvider string            `json:"AuthProvider"`
	StorageJSON  []byte            `json:"StorageJSON"`
	Metadata     map[string]any    `json:"Metadata"`
	Attributes   map[string]string `json:"Attributes"`
	Host         hostConfigSummary `json:"Host"`
}

type authRefreshResponse struct {
	Auth             authData `json:"Auth"`
	NextRefreshAfter string   `json:"NextRefreshAfter,omitempty"`
}

// newOAuthClientFor is an indirection point so tests can pin the HTTP transport.
var newOAuthClientFor = clientFor

func clientFor(host hostConfigSummary) *oauthClient {
	return newOAuthClient(host.ProxyURL)
}

func handleAuthParse(request []byte) ([]byte, error) {
	var req authParseRequest
	if err := decodeRequest(request, &req); err != nil {
		return errorEnvelope("bad_request", err.Error()), nil
	}
	if storageType(req.RawJSON) != authProviderID {
		return okEnvelope(authParseResponse{Handled: false})
	}
	storage, err := parseTokenStorage(req.RawJSON)
	if err != nil {
		return errorEnvelope("bad_request", err.Error()), nil
	}
	fileName := strings.TrimSpace(req.FileName)
	if fileName == "" {
		fileName = credentialFileName(storage.Email, storage.Subject)
	}
	data, err := buildAuthData(storage, fileName)
	if err != nil {
		return errorEnvelope("bad_request", err.Error()), nil
	}
	return okEnvelope(authParseResponse{Handled: true, Auth: *data})
}

func handleLoginStart(request []byte) ([]byte, error) {
	var req loginStartRequest
	if err := decodeRequest(request, &req); err != nil {
		return errorEnvelope("bad_request", err.Error()), nil
	}
	client := newOAuthClientFor(req.Host)
	deviceCode, err := client.requestDeviceCode(context.Background())
	if err != nil {
		return errorEnvelope("login_start_failed", err.Error()), nil
	}
	state, err := randomState()
	if err != nil {
		return errorEnvelope("login_start_failed", fmt.Sprintf("generate login state: %v", err)), nil
	}
	interval := time.Duration(deviceCode.Interval) * time.Second
	if interval < defaultPollInterval {
		interval = defaultPollInterval
	}
	now := time.Now()
	expiresAt := now.Add(time.Duration(deviceCode.ExpiresIn) * time.Second)
	if deviceCode.ExpiresIn <= 0 {
		expiresAt = now.Add(30 * time.Minute)
	}
	loginURL := strings.TrimSpace(deviceCode.VerificationURIComplete)
	if loginURL == "" {
		loginURL = strings.TrimSpace(deviceCode.VerificationURI)
	}
	metadata := map[string]any{
		"login_device_code":      deviceCode.DeviceCode,
		"login_token_endpoint":   deviceCode.TokenEndpoint,
		"login_interval_seconds": formatSeconds(interval),
		"login_expires_at":       expiresAt.UTC().Format(time.RFC3339),
		"login_user_code":        deviceCode.UserCode,
	}
	sessions.put(state, &loginSession{
		DeviceCode:    deviceCode.DeviceCode,
		TokenEndpoint: deviceCode.TokenEndpoint,
		Interval:      interval,
		ExpiresAt:     expiresAt,
		NextAttempt:   now,
	})
	return okEnvelope(loginStartResponse{
		Provider:  authProviderID,
		URL:       loginURL,
		State:     state,
		ExpiresAt: expiresAt.UTC(),
		Metadata:  metadata,
	})
}

func handleLoginPoll(request []byte) ([]byte, error) {
	var req loginPollRequest
	if err := decodeRequest(request, &req); err != nil {
		return errorEnvelope("bad_request", err.Error()), nil
	}
	state := strings.TrimSpace(req.State)
	session, ok := sessions.get(state)
	if !ok {
		// Rebuild the session from the host-registered metadata after a reload.
		session = sessionFromMetadata(req.Metadata)
		if session == nil {
			return errorEnvelope("unknown_state", "unknown or expired login state"), nil
		}
		sessions.put(state, session)
	}
	now := time.Now()
	if now.After(session.ExpiresAt) {
		sessions.delete(state)
		return okEnvelope(loginPollResponse{Status: "error", Message: "xai device code expired"})
	}
	if now.Before(session.NextAttempt) {
		return okEnvelope(loginPollResponse{Status: "pending"})
	}
	client := newOAuthClientFor(req.Host)
	outcome := client.exchangeDeviceCode(context.Background(), session.TokenEndpoint, session.DeviceCode, session.Interval)
	switch outcome.Status {
	case "pending":
		session.NextAttempt = now.Add(outcome.NextInterval)
		sessions.put(state, session)
		return okEnvelope(loginPollResponse{Status: "pending"})
	case "slow_down":
		session.Interval = outcome.NextInterval
		session.NextAttempt = now.Add(outcome.NextInterval)
		sessions.put(state, session)
		return okEnvelope(loginPollResponse{Status: "pending", Message: "xAI asked the login poll to slow down"})
	case "error":
		sessions.delete(state)
		return okEnvelope(loginPollResponse{Status: "error", Message: outcome.Message})
	default:
		sessions.delete(state)
		storage := buildTokenStorage(outcome.Token, session.TokenEndpoint)
		data, err := buildAuthData(storage, credentialFileName(storage.Email, storage.Subject))
		if err != nil {
			return errorEnvelope("login_failed", err.Error()), nil
		}
		return okEnvelope(loginPollResponse{Status: "success", Message: "xAI login complete", Auth: *data})
	}
}

func handleAuthRefresh(request []byte) ([]byte, error) {
	var req authRefreshRequest
	if err := decodeRequest(request, &req); err != nil {
		return errorEnvelope("bad_request", err.Error()), nil
	}
	storage, err := parseTokenStorage(req.StorageJSON)
	if err != nil {
		return errorEnvelope("bad_request", err.Error()), nil
	}
	if strings.TrimSpace(storage.RefreshToken) == "" {
		return errorEnvelope("refresh_failed", "xai-oauth: stored credential has no refresh token"), nil
	}
	tokenEndpoint := strings.TrimSpace(storage.TokenEndpoint)
	if tokenEndpoint == "" {
		tokenEndpoint = metadataString(req.Metadata, "token_endpoint")
	}
	client := newOAuthClientFor(req.Host)
	token, err := client.refreshTokens(context.Background(), storage.RefreshToken, tokenEndpoint)
	if err != nil {
		return errorEnvelope("refresh_failed", err.Error()), nil
	}
	// Preserve user-edited routing fields and roll the access token forward.
	usingAPI := storage.UsingAPI
	oldRefreshToken := storage.RefreshToken
	explicitBaseURL := strings.TrimRight(strings.TrimSpace(storage.BaseURL), "/")
	if strings.EqualFold(explicitBaseURL, defaultAPIBaseURL) || strings.EqualFold(explicitBaseURL, cliChatProxyBaseURL) {
		explicitBaseURL = ""
	}
	storage = buildTokenStorage(token, tokenEndpoint)
	// xAI's token endpoint omits refresh_token when it does not rotate; keep the
	// stored one so the credential stays refreshable.
	if strings.TrimSpace(storage.RefreshToken) == "" {
		storage.RefreshToken = oldRefreshToken
	}
	storage.UsingAPI = usingAPI
	if explicitBaseURL != "" {
		storage.BaseURL = explicitBaseURL
	}
	fileName := strings.TrimSpace(req.AuthID)
	if fileName == "" || !strings.HasSuffix(fileName, ".json") {
		fileName = credentialFileName(storage.Email, storage.Subject)
	}
	data, err := buildAuthData(storage, fileName)
	if err != nil {
		return errorEnvelope("refresh_failed", err.Error()), nil
	}
	return okEnvelope(authRefreshResponse{
		Auth:             *data,
		NextRefreshAfter: data.NextRefreshAfter,
	})
}

// sessionFromMetadata rebuilds a login session from the metadata registered at StartLogin.
func sessionFromMetadata(metadata map[string]any) *loginSession {
	deviceCode := metadataString(metadata, "login_device_code")
	if deviceCode == "" {
		return nil
	}
	session := &loginSession{
		DeviceCode:    deviceCode,
		TokenEndpoint: metadataString(metadata, "login_token_endpoint"),
		Interval:      defaultPollInterval,
	}
	if seconds, err := parseInt64(metadataString(metadata, "login_interval_seconds")); err == nil && seconds > 0 {
		session.Interval = time.Duration(seconds) * time.Second
	}
	if expiresAt, err := time.Parse(time.RFC3339, metadataString(metadata, "login_expires_at")); err == nil {
		session.ExpiresAt = expiresAt
	} else {
		session.ExpiresAt = time.Now().Add(30 * time.Minute)
	}
	session.NextAttempt = time.Now()
	return session
}

func randomState() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func formatSeconds(d time.Duration) string {
	return strconv.Itoa(int(d / time.Second))
}

func parseInt64(value string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(value), 10, 64)
}
