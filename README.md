# cpa-plugin-xai-oauth

xAI (Grok) **OAuth device-flow login** and **per-account model discovery**
as an external dynamic-library plugin for
[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). It ships as a
separate provider key `xai-oauth` and coexists with the built-in `xai`
provider (which continues to serve `xai-api-key` config entries and the
Responses-API/websocket/chat-proxy special paths).

## Capabilities

| Capability | RPC methods | What it does |
| --- | --- | --- |
| `AuthProvider` | `auth.identifier` / `auth.parse` / `auth.login.start` / `auth.login.poll` / `auth.refresh` | OIDC discovery, RFC 8628 device-code login, token refresh |
| `ModelProvider` | `model.static` / `model.for_auth` | Account model discovery from `/models` with Grok CLI identity headers, merged with the embedded static catalog |

Inference and credential bearer injection run through the host's built-in
OpenAI-compatible executor path automatically (the plugin declares no
`Executor` capability): `Attributes["base_url"]` + `Attributes["api_key"]`
route requests, and `header:*` attributes carry the Grok CLI identity headers
(`X-XAI-Token-Auth`, `x-grok-client-version`, `x-grok-client-identifier`,
`User-Agent`).

## Build & install

Requires Go 1.26+ with CGO enabled.

```bash
make build   # runs vet + tests, then builds bin/xai-oauth-v<version>.<ext>
make install PLUGINS_DIR=/path/to/cliproxyapi/plugins
```

`install` copies the artifact into `<PLUGINS_DIR>/<goos>/<goarch>/`, the
layout the host discovery supports (`plugins.dir` defaults to `plugins`
relative to the server working directory). You can also just drop the file
into the plugins directory directly. The artifact name follows the host's
plugin file convention (`xai-oauth-v0.1.0.dylib` on macOS, `.so` on Linux,
`.dll` on Windows).

## Enable

The host auto-loads discovered plugins only when enabled in `config.yaml`:

```yaml
plugins:
  enabled: true
  dir: "plugins"          # default; looks for plugins/<goos>/<goarch>/*.dylib|.so
  configs:
    xai-oauth:
      enabled: true
      priority: 1
```

Restart (or wait for the hot reload) and check the log for plugin loading.

## Login

Plugin providers get a generic management login endpoint automatically:

```
GET /v0/management/xai-oauth-auth-url
```

Returns `{ "url": "...", "state": "..." }`. Open the URL in a browser to
authorize (Grok device flow), then poll
`GET /v0/management/get-auth-status?state=<state>` until `status=ok`. The
credential is saved to `auths/xai-oauth-<email>.json` with
`type: "xai-oauth"`, `auth_kind: oauth`, and
`base_url: https://api.x.ai/v1`; the host routes chat to the Grok CLI
chat proxy by default (mirroring the built-in OAuth behavior, via attributes).

The management WebUI lists plugin providers on its OAuth login page.

## Model discovery

`model.for_auth` fetches `<base>/models` with the Grok CLI identity headers
and merges the result with the embedded static catalog (`catalog.json`,
trimmed from `internal/registry/models/models.json`). Discovered
`reasoning_efforts` map to thinking levels. Failures are reported to the
host, which then skips registration for that auth (fail-closed).

Toggling is supported via the credential file:

- `"using_api": true` — route to `https://api.x.ai/v1` (no CLI identity headers).
- explicit custom `"base_url"` (any URL other than the two known bases) — use it as-is.

## Storage compatibility

The persisted JSON intentionally mirrors the built-in xAI credential shape
(`access_token`, `refresh_token`, `id_token`, `expired`, `last_refresh`,
`email`, `sub`, `base_url`, `token_endpoint`, `auth_kind`, `using_api`).
Files with `type: "xai-oauth"` are parsed by this plugin; files with
`type: "xai"` belong to the built-in provider and are explicitly not claimed
by the plugin parser.

## Known limitations (compared to the built-in xai provider)

- Chat goes through the generic OpenAI-compatible path
  (`/chat/completions`), not the Responses API. The Grok CLI chat proxy
  accepts both endpoints, but Responses-only features are unavailable:
  `/responses/compact`, reasoning `encrypted_content` replay, x_search tool
  filtering, free-usage cooldown handling, and downstream websocket
  passthrough (`/v1/responses` websocket mode).
- Media generation routes (`/v1/videos`, image generation) are not exposed
  for `xai-oauth` auths; use the built-in provider or an xAI API key for those.
- The Grok Shell (`grokbuild`) UA model listing is not served for
  `xai-oauth` (it filters to built-in `xai` auths).
- If the Grok client version (`grokClientVersion` in `oauth.go`) drifts from
  what chat-proxy expects, identity-header requests may fail; bump it to the
  same value the core uses (`internal/runtime/executor/xai_executor.go`).

## Files

| File | Purpose |
| --- | --- |
| `main.go` | cgo entry point (`cliproxy_plugin_init`), buffer marshalling |
| `register.go` | method dispatch + registration payload |
| `rpc.go` | wire types mirroring `sdk/pluginapi` structs |
| `oauth.go` | OIDC discovery, device flow, refresh (ported from `internal/auth/xai`) |
| `auth.go` | `auth.*` handlers + login session store |
| `storage.go` | credential JSON shape, AuthData assembly |
| `models.go` | `model.for_auth` discovery + catalog merge (ported from `sdk/cliproxy/xai_models.go`) |
| `catalog.go` / `catalog.json` | embedded static model metadata |
| `Makefile` | build/install |

## Credits & license

The OAuth and model-discovery logic is ported from
[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (`internal/auth/xai`
and `sdk/cliproxy/xai_models.go`); the plugin protocol follows its
`sdk/pluginapi` / `examples/plugin` contract. Distributed under the MIT
license (see [LICENSE](LICENSE)).
