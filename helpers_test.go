package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// setDefaultPollInterval overrides the RFC 8628 polling floor in tests.
func setDefaultPollInterval(d time.Duration) {
	defaultPollInterval = d
}

// envelopeResult decodes an RPC envelope and returns the result payload.
func envelopeResult(t *testing.T, raw []byte) []byte {
	t.Helper()
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("expected ok envelope, got error %s: %s", env.Error.Code, env.Error.Message)
	}
	return env.Result
}

func httptestNewTokenServer(t *testing.T, accessToken string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"access_token": accessToken,
			"expires_in":   3600,
		})
	}))
}

func bytesContain(raw []byte, substr string) bool {
	return strings.Contains(string(raw), substr)
}
