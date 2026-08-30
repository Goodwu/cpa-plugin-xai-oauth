package main

import (
	"bufio"
	"bytes"
	"strings"
)

// pluginPrefix is the model prefix this plugin's auths are exposed under. It
// is populated from the host plugin config (plugins.configs.<pluginID>) on
// register/reconfigure, and from auth-file prefix fields, then applied to
// auth data in buildAuthData. When empty the auth stays unprefixed.
var pluginPrefix string

// setPluginConfig applies the host-supplied plugin configuration block
// (the plugins.configs.<pluginID> YAML serialized by the host). We only need
// the "prefix" key; the block is simple YAML, so a line scan avoids pulling a
// YAML dependency into the module.
func setPluginConfig(raw []byte) {
	pluginPrefix = strings.TrimSpace(yamlScalar(raw, "prefix"))
}

// yamlScalar extracts the scalar value of a top-level "key: value" line from
// a small YAML block. Multiline/array values are out of scope here.
func yamlScalar(raw []byte, key string) string {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	prefix := key + ":"
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		if rest == "" {
			continue
		}
		// Drop an inline comment, then surrounding quotes.
		if idx := strings.Index(rest, " #"); idx >= 0 {
			rest = rest[:idx]
		}
		rest = strings.TrimSpace(rest)
		if len(rest) >= 2 {
			if (rest[0] == '"' && rest[len(rest)-1] == '"') ||
				(rest[0] == '\'' && rest[len(rest)-1] == '\'') {
				rest = rest[1 : len(rest)-1]
			}
		}
		return strings.TrimSpace(rest)
	}
	return ""
}