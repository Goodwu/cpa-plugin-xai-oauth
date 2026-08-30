package main

import (
	"encoding/json"
	"fmt"
)

const (
	// pluginID is the provider identifier exposed via auth.identifier and the
	// provider key stamped on auth records. It deliberately differs from the
	// built-in "xai" provider so the plugin can coexist with it.
	pluginID = "xai-oauth"
	// schemaVersion is the RPC contract version this plugin speaks.
	schemaVersion = 1
)

// pluginVersion is reported in registration metadata and file names. The
// Makefile overrides it at link time via -ldflags -X; the literal is a
// fallback for plain `go build` invocations.
var pluginVersion = "0.1.0"

type registrationResponse struct {
	SchemaVersion uint32         `json:"schema_version"`
	Metadata      pluginMetadata `json:"metadata"`
	Capabilities  struct {
		AuthProvider  bool `json:"auth_provider"`
		ModelProvider bool `json:"model_provider"`
	} `json:"capabilities"`
}

func registrationPayload() registrationResponse {
	resp := registrationResponse{
		SchemaVersion: schemaVersion,
		Metadata: pluginMetadata{
			Name:             pluginID,
			Version:          pluginVersion,
			Author:           "Goodwu",
			GitHubRepository: "https://github.com/Goodwu/cpa-plugin-xai-oauth",
		},
	}
	resp.Capabilities.AuthProvider = true
	resp.Capabilities.ModelProvider = true
	return resp
}

// dispatch routes a host RPC method to its handler.
func dispatch(method string, request []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		return okEnvelope(registrationPayload())
	case "plugin.shutdown":
		return okEnvelope(struct{}{})
	case "auth.identifier":
		return okEnvelope(map[string]string{"identifier": pluginID})
	case "auth.parse":
		return handleAuthParse(request)
	case "auth.login.start":
		return handleLoginStart(request)
	case "auth.login.poll":
		return handleLoginPoll(request)
	case "auth.refresh":
		return handleAuthRefresh(request)
	case "model.static":
		return handleStaticModels(request)
	case "model.for_auth":
		return handleModelsForAuth(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func decodeRequest(request []byte, target any) error {
	if len(request) == 0 {
		return nil
	}
	if err := json.Unmarshal(request, target); err != nil {
		return fmt.Errorf("decode plugin request: %w", err)
	}
	return nil
}
