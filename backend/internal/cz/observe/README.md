# P6: read-only observe routes for the CodizeLabs dashboard

CodizeLabs fork patch P6 of grok2api. It lets the CodizeLabs generation layer's dashboard show grok2api's Grok account pool, usage, recent requests and proxy health, without holding grok2api's admin login. P6 is ours and is never offered upstream.

**Ruling:** the CTO's ruling of 4 November 2026 in the AIFriend-gen repository, `DECISIONS.md` D36, with ADR-0001's review log ("the fork rule is extended by P6, read-only"). The design is in `docs/design/generation-layer/consult/ai-juno-one-dashboard.md` § 4 there. ADR-0001 B2 still holds: the layer never gets the admin login, and P6's token can't export or change anything.

## Endpoints

Every route is `GET` under `/cz/v1/observe` on grok2api's own router and port. No new listener is added. Every response carries `Cache-Control: no-store`.

| Path | What it returns |
|---|---|
| `/overview?limit=1..100` | everything below in one read (summary, accounts, 24 h usage, the last `limit` requests, proxies); a part whose source fails is `null` and named in `errors` |
| `/accounts` | every account (up to 2000): `label`, `provider`, `identity_masked`, `tier`, `state` (`active`, `cooling`, `disabled`, `failed`), `reason` (`cooldown`, `quota`, `config`, `auth`), `until`, `enabled`, `auth_status`, `priority`, `max_concurrent`, `failure_count`, `cooldown_until`, `last_error_code`, `last_used_at`, `quota` (grok2api's own summary) and `quota_windows` (`mode`, `remaining`, `total`, `usage_percent`, `window_s`, `reset_at`, `synced_at`, `source`) |
| `/summary` | grok2api's account counts: total, available, recovering, attention, cool-down, waiting for reset, probing, disabled, re-auth needed, per provider |
| `/usage?period=24h\|7d` | requests, successful, failed, `success_rate`, `avg_first_token_ms`, a request series and the per-provider split |
| `/requests?limit=1..100` | the newest requests: time, operation, public model id, provider, account label, status, `ok`, error code, duration, first-token time, attempts, streaming, output images and seconds |
| `/proxies` | every egress node: `label`, scope, enabled, `state` (`healthy`, `unhealthy`, `unknown`, `cooling`, `disabled`), health, probe status and latency, last probe, failures, cool-down, assigned accounts, capacity, last error code; and counts. Empty in direct mode |

**What P6 never returns:** a token, cookie, credential (encrypted or not), e-mail, user id, team id, account name, client key, client IP, request header, request or response body, request path, upstream URL, prompt text, proxy URL, proxy name or exit IP. An account is its opaque label (`acct_` plus 16 hex, P3's keyed HMAC of the account id, so P3 and P6 show the same label) plus `ab…yz`, the first two and last two characters of its e-mail or name. A proxy is `proxy_` plus 8 hex. grok2api's free-text last errors become short codes (`rate_limited`, `quota`, `auth`, `forbidden`, `statsig`, `challenge`, `timeout`, `network`, `upstream_error`, `error`). Every view is built field by field, so a field upstream adds later never appears.

## Auth and failure modes

| Case | Answer |
|---|---|
| any write method (`POST`, `PUT`, `PATCH`, `DELETE`, `OPTIONS`, …) on any path | **405** `method_not_allowed`, `Allow: GET`, before anything else is checked |
| `GROK2API_OBSERVE_TKN` unset, or shorter than 32 characters | grok2api starts as before and logs one warning `cz_observe_unavailable` (never the value); every read is **503** `observe_not_configured` |
| no `Authorization: Bearer <observe token>`, or a wrong one (the admin login and client keys included) | **401** `observe_unauthorized` (constant-time compare) |
| a source that fails | **503** `source_unavailable` or **500** `read_failed`; in `/overview`, that part is `null` |

The token comes only from the environment variable `GROK2API_OBSERVE_TKN` (Doppler in production; `ops/generation/up.sh` makes one per start when Doppler has none). It opens `/cz/v1/observe/*` and nothing else.

## Files

- `observe.go`: the views, the mask and the error codes.
- `handler.go`: the routes, the 405s, the token check.
- `../../app/cz_observe.go`: built at startup, feature-only.
- `../../transport/http/cz_observe.go`: mounted beside P3. Shared upstream lines: one field pair in `Dependencies` and one `registerCZObserve` call (`server.go`), one constructor line and two fields in `application.go`.

## Tests

`go test ./internal/cz/observe/ ./internal/app/ ./internal/transport/http/ -run 'CZObserve|Observe|Token|Write|Unconfigured|Account|Usage|Mask|Overview|NoCredential'`

| Test | Proves |
|---|---|
| `TestNoCredentialOrPromptInAnyResponse` | every string field of every account, billing, linked account, audit record, attempt and proxy node (found by reflection, so new upstream fields are covered) is a canary, and no canary, e-mail, token, header, IP or the word "prompt" appears in any response |
| `TestWriteMethodsAre405` | read-only, with and without the token, configured or not |
| `TestTokenIsRequiredAndScoped` | only the observe token opens the routes |
| `TestUnconfiguredIs503`, `TestCZObserveIsFeatureOnly` | a missing or short token never crashes grok2api, logs a warning, never logs the value |
| `TestCZObserveRoutesAreMountedReadOnlyAndUnconfiguredByDefault` | mounted on grok2api's router; **red on the base** `cz-e2fd7ab-p5` (404), green on P6 |
| `TestAccountStatesAndMasks`, `TestUsageRequestsProxiesAndSummary`, `TestOverviewNamesAFailingSource`, `TestMaskAndErrorCode` | states, masks, labels, usage maths, limits, overview |

**Red proof:** `./redproof.sh` breaks each guard in turn (the mask returns the e-mail; the free-text error passes through; the request log shows the account name; a proxy shows its URL; the 405s removed; an empty token accepted; any bearer accepted; P6 on with no token; P6 not mounted), checks that the broken code still compiles, runs the guarding test and expects red, then restores the file. It exits 0 only when every break is caught.
