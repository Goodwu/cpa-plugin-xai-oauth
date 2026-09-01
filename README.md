# cpa-plugin-xai-oauth

xAI (Grok) **OAuth device-flow login** and **per-account model discovery**
as an external dynamic-library plugin for
[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). It ships as a
separate provider key `xai-oauth` and coexists with the built-in `xai`
provider (which continues to serve `xai-api-key` config entries and the
Responses-API/websocket/chat-proxy special paths). Its auth identity is
`xai` — the same provider string the built-in xAI auth uses — so credential
files and management-panel rendering match the built-in provider, while
model registration keeps the distinct `xai-oauth` key ("Storage
compatibility" below).

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

Requires Go 1.26+ with CGO enabled (Linux release builds additionally
require Docker).

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

The version is derived from git by default (`make` helpfully stamps the
build): an exact release tag yields that tag (`v0.1.3` → `0.1.3`), otherwise
the nearest tag plus the short commit id (`v0.1.2-2-gd01dfda` →
`0.1.2-gd01dfda`; the describe commit-count segment is dropped so dev builds
stay correctly ordered against release tags). Dirty trees get a `-dirty`
suffix, and `make build VERSION=…` always overrides. It is injected at link
time via `-ldflags -X`, so the registration metadata, the artifact file name,
and the release assets always agree. CI passes `VERSION=` explicitly
(tag releases use the tag; the CI matrix builds as `0.0.0`).

Release packaging for the four supported platforms:

```bash
make package-linux-amd64   # builds inside Docker (glibc-pinned image)
make package-linux-arm64
make package-darwin-amd64  # requires a Darwin host
make package-darwin-arm64
# -> dist/xai-oauth_<version>_<goos>_<arch>.zip + dist/checksums.txt
```

Each zip contains only the shared library (root-level file name), matching
the host's plugin store layout.

## CI and releases

- `push`/`pull_request` runs the CI workflow: `go vet` + `go test` on
  Linux, plus a four-platform package matrix (linux/amd64, linux/arm64,
  darwin/amd64, darwin/arm64) that uploads the zips as artifacts. Pushes
  never publish a release.
- Pushing a semver tag `vX.Y.Z` runs the release workflow: validates the
  tag, builds all four platforms, and creates a GitHub Release containing
  the four platform zips plus `checksums.txt`.

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
`type: "xai"`, `auth_kind: oauth`, and
`base_url: https://api.x.ai/v1`; the host routes chat to the Grok CLI
chat proxy by default (mirroring the built-in OAuth behavior, via attributes).

The management WebUI lists plugin providers on its OAuth login page; because
the auth identity is `xai`, plugin credentials render with the built-in xAI
theme (solid card outlines) instead of the generic dashed fallback.

## Model discovery

Models are discovered **per account** and the list is account-filtered:
`model.for_auth` fetches `<base>/models` with the Grok CLI identity headers
(the chat-proxy returns only the models the signed-in Grok account can
actually use) and merges each with the embedded static catalog
(`catalog.json`, trimmed from `internal/registry/models/models.json`) to
backfill metadata such as context length. Discovered `reasoning_efforts`
map to thinking levels. Failures are reported to the host, which then skips
registration for that auth (fail-closed).

`model.static` deliberately returns an empty list: the host registers static
models globally, independent of any credential, so publishing the catalog
there would surface models the current account cannot use (they would appear
in `/v1/models` but fail at auth selection). Request routing is unaffected
either way — auth selection always gates on the per-auth registration. The
practical consequence is that no `xai-oauth` models appear until you log in.

The `model.for_auth` response carries `Provider: "xai"` (the credential's
auth identity), not the `xai-oauth` model key: the host discards per-auth
results whose provider does not match the auth identity and silently falls
back to the built-in static xAI model list, which would defeat the filtering.

The credential `prefix` (host `force-model-prefix: true`) controls the
served model IDs: with `prefix: "xai"` the base ID is dropped and models are
only reachable under the `xai/<model>` alias (e.g. `xai/grok-4.6`).

Toggling is supported via the credential file:

- `"using_api": true` — route to `https://api.x.ai/v1` (no CLI identity headers).
- explicit custom `"base_url"` (any URL other than the two known bases) — use it as-is.

## Storage compatibility

The persisted JSON intentionally mirrors the built-in xAI credential shape
(`access_token`, `refresh_token`, `id_token`, `expired`, `last_refresh`,
`email`, `sub`, `base_url`, `token_endpoint`, `auth_kind`, `using_api`).

Because the auth identity is `xai`, the host binds the **built-in** xAI
executor chain to these auths — for inference and for credential refresh
alike. That chain reads its inputs from auth *metadata*, not the storage
JSON, so `buildAuthData` mirrors `access_token`, `refresh_token`, `expired`,
and `last_refresh` into `Metadata` on every parse/login/refresh. Without the
mirror, `XAIExecutor.Refresh` finds no `refresh_token`, returns the auth
unchanged as a silent "success", the 401 retry gate (`authHasRefreshToken`)
never fires, and tokens expire in place. For the same reason the attributes
deliberately omit `api_key`: `xaiCreds()` prefers the attribute over the
metadata token, and attributes are only rebuilt on parse, so a pinned
`api_key` would keep sending the stale bearer after a refresh. Routing
attributes (`base_url`, `using_api`, Grok CLI identity headers) stay in
`Attributes`; the chat-proxy identity headers there remain correct because
the built-in executor re-applies its own set from `using_api` + `base_url`.

The plugin's auth identity is `xai`: `auth.identifier` and the `Provider`
stamped on saved credentials both report `xai`, and files are written as
`xai-oauth-<email>.json` with `type: "xai"`. The host matches an auth file
to a plugin by exactly this string on every path (file loading, refresh,
login polling, model discovery, executor wrapping, management panel), so
the two must agree. While the plugin is enabled it therefore owns
`type: "xai"` credential files; the built-in `xai` provider still handles
`xai-api-key` config entries and its Responses-API/websocket/chat-proxy
special paths. Model registration keeps the distinct `xai-oauth` provider
key, so routing never depends on the auth identity.

Upgrading from v0.1.1 (or older): files written by those releases carry
`type: "xai-oauth"` and are no longer claimed by the plugin, so they stop
appearing in the management panel. Rewrite the `type` field to `"xai"` in
each file (the host restamps it on the next refresh/login), or delete them
and log in again.

## Known limitations (compared to the built-in xai provider)

- None on the inference path anymore: since the auth identity is `xai`, the
  host binds the built-in xAI executor chain (`XAIAutoExecutor` →
  Responses API on the Grok CLI chat proxy, with compact, reasoning replay,
  x_search handling, cooldown mapping, and 401 refresh-retry) to plugin
  auths. The plugin itself only owns login, parsing, refresh RPC, and model
  discovery. The built-in executor's Grok client-version constants live in
  `internal/runtime/executor/xai_executor.go`; the plugin keeps its own
  `grokClientVersion` in `oauth.go` in sync for its direct API calls
  (device flow, `/models`).
- Media generation routes (`/v1/videos`, image generation) are not exposed
  for `xai-oauth` auths; use the built-in provider or an xAI API key for those.
- The Grok Shell (`grokbuild`) UA model listing is not served for
  `xai-oauth` (it filters to built-in `xai` auths).

A prioritized plan to port the built-in Responses-API path into this plugin
(executor capability, task breakdown, LOC/effort estimates, and the
build-vs-skip decision lines) lives in
[`docs/PLAN-xai-responses-executor.md`](docs/PLAN-xai-responses-executor.md).
It is analysis-only: nothing there is implemented yet.

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
| `Makefile` | build/install + 4-platform release packaging |
| `scripts/package-release.sh` | zip + sha256 packaging for release assets |
| `.github/workflows/ci.yml` | CI: vet/test + 4-platform package matrix |
| `.github/workflows/release.yml` | tag-triggered GitHub Release publishing |

## Credits & license

The OAuth and model-discovery logic is ported from
[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (`internal/auth/xai`
and `sdk/cliproxy/xai_models.go`); the plugin protocol follows its
`sdk/pluginapi` / `examples/plugin` contract. Distributed under the MIT
license (see [LICENSE](LICENSE)).
