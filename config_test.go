package main

import (
	"testing"
)

func TestSetPluginConfigPrefix(t *testing.T) {
	pluginPrefix = ""
	setPluginConfig([]byte("enabled: true\npriority: 1\nprefix: \"xai\"\n"))
	if pluginPrefix != "xai" {
		t.Fatalf("expected prefix xai, got %q", pluginPrefix)
	}
}

func TestSetPluginConfigEmptyOrMissing(t *testing.T) {
	pluginPrefix = "old"
	setPluginConfig([]byte("enabled: true\npriority: 1\n"))
	if pluginPrefix != "" {
		t.Fatalf("missing prefix should leave pluginPrefix empty, got %q", pluginPrefix)
	}
	setPluginConfig(nil)
	if pluginPrefix != "" {
		t.Fatalf("nil config should leave pluginPrefix empty, got %q", pluginPrefix)
	}
}

func TestYAMLScalar(t *testing.T) {
	raw := []byte("enabled: true\nprefix: xai\n# comment\nexcluded: [a, b]\n")
	if got := yamlScalar(raw, "prefix"); got != "xai" {
		t.Fatalf("expected xai, got %q", got)
	}
	if got := yamlScalar(raw, "enabled"); got != "true" {
		t.Fatalf("expected true, got %q", got)
	}
	if got := yamlScalar(raw, "missing"); got != "" {
		t.Fatalf("expected empty for missing key, got %q", got)
	}
	if got := yamlScalar([]byte("prefix: \"quoted-value\" # trailing"), "prefix"); got != "quoted-value" {
		t.Fatalf("expected quoted-value with comment stripped, got %q", got)
	}
}

func TestBuildAuthDataAppliesPrefix(t *testing.T) {
	pluginPrefix = "xai"
	defer func() { pluginPrefix = "" }()
	storage := buildTokenStorage(sampleToken(), "https://auth.x.ai/token")
	data, err := buildAuthData(storage, "xai-oauth-user@example.com.json")
	if err != nil {
		t.Fatalf("buildAuthData: %v", err)
	}
	if data.Prefix != "xai" {
		t.Fatalf("expected pluginPrefix applied to AuthData.Prefix, got %q", data.Prefix)
	}
	if got, ok := data.Metadata["prefix"]; !ok || got != "xai" {
		t.Fatalf("expected prefix in metadata, got %v", data.Metadata["prefix"])
	}
}

func TestBuildAuthDataAuthFilePrefixWins(t *testing.T) {
	pluginPrefix = "xai"
	defer func() { pluginPrefix = "" }()
	storage := buildTokenStorage(sampleToken(), "https://auth.x.ai/token")
	storage.Prefix = "custom-xai"
	data, err := buildAuthData(storage, "xai-oauth-user@example.com.json")
	if err != nil {
		t.Fatalf("buildAuthData: %v", err)
	}
	if data.Prefix != "custom-xai" {
		t.Fatalf("expected auth-file prefix to win, got %q", data.Prefix)
	}
}