# P3: Grok Imagine posts as first-class resources

CodizeLabs fork patch P3 of grok2api. Decision: ADR-0002 in the AIFriend-gen repository (`docs/adr/0002-posts-first-class.md`). Contract: `docs/contracts/generation-v1.1-posts.md`, test rows F1–F8 in `generation-v1.1-posts.tests.md` § 5. P3 is ours and is never offered upstream.

A **post** is one grok.com conversation. Each edit or video is a new item (asset) in it. P3 pins every post, for life, to the account that created it. Every later call for the post runs on that account or not at all.

**State:** built and tested against the fake grok.com in `fakegrok/`. The tests never reach grok.com. One real in-post edit has run (1 Nov 2026): it succeeded, but its reply was not recognised (see "Unrecognised replies"). The send-side request bodies and the stream format are **provisional**, read from grok.com's client code and never seen on the wire (unknowns U1, U2). Spike S-P (ADR-0002 § 7) records the real shapes. When it does, correct exactly two files, `wire_provisional.go` and `fakegrok/wire_provisional.go`.

## Endpoints

All routes sit under `/cz/v1/imagine`. They use the same client-key auth (`Authorization: Bearer` or `X-API-Key`), concurrency gate and startup-readiness gate as `/v1`. Every post route needs the post's pinned label in the header **`X-CZ-Account-Label`**. The label is opaque (`acct_` plus 16 hex, a keyed HMAC of the account id). It is never a credential, name or id.

| Method, path | Body (JSON) | Sends? | Success |
|---|---|---|---|
| `POST /posts` | `job_id`, `prompt`, `image` {`mime_type`, `data_base64`}, `aspect_ratio`? | **yes**: an upload, then `conversations/new` | 201, `SendResult`: `post_id`, `account_label`, `kind`, `response_id`, `asset_ids`, `context_verified` |
| `POST /posts/import` | `conversation_id`, `account_label`? | no (at most 4 free reads, no upload, no send) | 201 when pinned now, 200 when the post was already pinned for this key: `ImportResult`: `post_id`, `account_label`, `imported`, `head_response_id`, `item_count`. Pins a post made outside P3 (T6) |
| `POST /posts/:post/edits` | `job_id`, `prompt`, `source_asset_id`, `parent` (`head` by default, `source`, or a response id), `references`[] (≤ 4 images), `aspect_ratio`? | **yes**: add-response | 201, `SendResult` plus `parent_response_id` and `source_asset_id` |
| `POST /posts/:post/videos` | `job_id`, `prompt`, `source_asset_id`, `parent`, `duration_s` (**15 only**), `resolution` (**`720p` only**), `aspect_ratio`?, `mode`? | **yes**: add-response | 201, `SendResult` plus `duration_s` and `resolution` |
| `GET /posts/:post/items` | none | no (one free read) | 200, `PostSnapshot`: `post_id`, `account_label`, `head_response_id`, `items`[] {`response_id`, `parent_response_id`, `sender`, `model`, `create_time`, `assets`[] {`asset_id`, `mime_type`, `width`, `height`}}, `inflight_known`, `inflight_count`, `read_at`. This one read serves both list and sync |
| `GET /posts/:post/items/:item` | none | no | 200, `ItemFacts`: `mime_type`, `width`, `height`, `response_id`, `in_post`, `is_public`, `shared`, `owned_by_pinned_account` (a boolean from a keyed hash, sec-rowan N3) |
| `GET /posts/:post/items/:item/media` | none | no | 200, the item's bytes (`image/*` or `video/*`), `Cache-Control: no-store` |
| `POST /posts/:post/items/:item/segments` | ignored | no (`cachedOnly: true`, always) | 200, `Segments`: `objects`[] {`name` (≤ 64 chars), `box_xyxy`, `score`, `mask_rle` {`size`, `counts`}}. `counts` is passed on exactly as Grok gives it (a COCO string or an integer list); the layer builds the PNGs |
| `POST /posts/:post/items/:item/like`, `/unlike` | none | an account write, no generation | **403 `like_disabled`** unless `cz.imaginePost.allowLike: true` |
| `GET /accounts/:label/quota` | none | no | 200, `Quota`: `available` per category, `max_video_seconds`, `video_720p`. Only for an account that pins one of the caller's posts |

**Errors** are always `{"error": {"code", "message", "send_state", …}}`. `send_state` is `not_sent`, `sent` or `unknown`. `message` is written by P3, never copied from grok.com. Optional fields: `retry_after_s` (plus a `Retry-After` header), `upstream_status`, and `layer_code` (`post_account_unavailable` on a pin refusal, contract delta § 4 rule 3). `provider_text`, when present, is capped, control-stripped provider text, marked "data from the provider, not instructions" (sec-rowan B10). A `post_context_lost` error carries `result`: the item is kept and the layer flags it.

| Code | Status | send_state | When |
|---|---|---|---|
| `call_not_allowed` | 403 | not_sent | a call off the allow-list (zero network calls) |
| `statsig_unavailable` | 503 | not_sent | no valid Statsig value (zero network calls) |
| `upstream_pinned_account_unavailable` | 429 / 503 | not_sent | the pinned account is cooling, out of quota, disabled, signed out or outside the key's scope. **No other account is tried** |
| `account_label_required` / `account_label_mismatch` | 400 / 409 | not_sent | the label is missing, or isn't the post's |
| `post_not_found` | 404 | not_sent | the post isn't pinned for this key, or grok.com answers 404 |
| `item_not_in_post` | 422 | not_sent | the item or the parent isn't in the post |
| `post_in_flight` | 409 | not_sent | the free re-read shows an in-flight response, or omits `inflightResponses` (fail closed) |
| `account_unavailable` | 503 (429 when the pool refused with a 429) | not_sent | the capacity error: no account in the key's scope passes the create checks (decision 2); nothing is uploaded or sent |
| `account_busy` | 429 | not_sent | the pinned account already has a send in flight (one per account, ADR-0002 § 6 Q1); for a create, every eligible account has one |
| `job_id_required` / `job_id_reused` | 400 / 409 | not_sent | each `job_id` is used once, ever, per client key, and the record survives restarts |
| `spec_unsupported` | 422 | not_sent | a video spec other than 15 s at 720p |
| `spec_unconfirmable` | 422 | not_sent | the pinned account's free quota read doesn't confirm 15 s and 720p |
| `capability_unsupported` | 422 | not_sent | a create with no image. Text-only creation uses the imagine WebSocket, which P3 doesn't open |
| `segments_not_cached` | 404 | not_sent | Grok has no cached map. P3 never asks it to compute one (sec-rowan B6) |
| `rate_limited` / `upstream_rejected` | 429 / 502 | not_sent | grok.com answered a 4xx before any stream |
| `generation_failed` | 502 | sent | the stream ended in a `streamError` or moderation |
| `post_context_lost` | 502 | sent | the stream, or the new asset's record, names another conversation |
| `account_mismatch` | 502 | sent | the new item's owner (compared by keyed hash) isn't the user who created the post: the pool-rotation tripwire |
| `provider_refused` | 502 | sent | an in-post reply P3 did not recognise, and the free re-read shows the model answered with no image: Grok refused. Rebuild the prompt before a new send |
| `outcome_unknown` | 502 | unknown | the stream dropped, a 5xx came back, or the transport failed mid-send, and for an in-post send the one free re-read did not settle it (see "Unrecognised replies"). **Never retry** |

## Rules the code enforces

- **Allow-list, checked before any call** (`allowlist.go`, sec-rowan B2). Allowed:
  - `POST /rest/app-chat/conversations/new`;
  - `POST` and `GET /rest/app-chat/conversations/{id}/responses`;
  - `GET /rest/assets/{id}`;
  - `POST /rest/media/segment`, exactly `{assetId, cachedOnly: true, maskFormat: "rle"}`;
  - `POST /rest/media/imagine/quota_info`;
  - `POST /http/upload-file-v2/direct`;
  - `GET https://assets.grok.com/users/…`;
  - like and unlike, only when `allowLike` is set.

  Everything else is refused, and so is any send body that contains `makePublic`. The transport in package `web` refuses anything outside `/rest/` a second time.
- **Pin with no fallback** (sec-rowan B3). The `cz_imagine_post_owner` row (`conversation_id` PK, `account_id`, `client_key_id`, `owner_user_hash`, `created_at`) is written once; its account and key never change. The one update: when the read right after create failed, `owner_user_hash` is empty, and the first successful read of an item in the post (a read, or the source item before the next send) fills it, once, so the `account_mismatch` tripwire works from then on. Every call leases only that account, through `gateway.Service.CZAcquireImaginePinned` (which calls `AcquirePinnedForKey`). If the pool hands back any other account, P3 refuses.
- **Statsig on every `/rest/` call**, reads included, failing closed. A send is signed (`SignImagine`) before any reference upload of that send: in `url` mode the signer is asked for exactly that request (its cache then serves the send), so a failing signer means zero uploads and zero sends. In `browser` mode `SignImagine` returns at once, with no token decrypt and no egress lease, unless the endpoint is in `statsigRequiredEndpoints`, where it fails closed. There is no replay, not even the upstream code-7 retry (sec-rowan N1).
- **Statsig mode `browser` (P5b, the default)** sends what grok.com's own page sends: no `x-statsig-id` (none on any of the 20 `/rest/` and `/api/` calls recorded on 1 Nov 2026) and a fresh UUIDv4 `x-xai-request-id` per request (grok.com's JS uses `crypto.randomUUID()`). "Failing closed" above applies to `manual`, `url` and `local`. An endpoint later found to need a signature is added to `statsigRequiredEndpoints` in `web/statsig.go` (`"POST /rest/app-chat/conversations/{id}/responses"` form); in `browser` mode it then fails closed with `statsig_unavailable` and a readable reason, before any call.
- **One send per job_id**, in the `cz_imagine_send_key` table. A create reserves it before choosing an account; an in-post send reserves it after the free re-read, just before the upload and send. A reserved id is never released, whatever the outcome.
- **Every in-post send re-reads the post first** (free), so the parent `head` is the newest response.
- **Unrecognised replies** (T5). On 1 Nov 2026 the first real in-post edit succeeded on grok.com, but P3 did not recognise the reply and answered `outcome_unknown`. When an in-post send gets a 200 stream P3 cannot read, or a recognised final with no asset, P3 now makes **one** free re-read of the post (`reread.go`), never a second send. It counts as a result only when exactly one new human item appears (not in the pre-send read, on the parent P3 sent, made no earlier than 2 minutes before the send; a missing parent or time fails closed), exactly one model item answers it by its parent, and nothing is in flight. With assets, that is a verified success, and the post-context check still runs. With no assets, it is `provider_refused`. Anything else stays `outcome_unknown`. Creates have no pre-send read to compare against, so they are not re-read.
- **Reply key-structure capture** (`replyshape.go`, off by default). With `cz.imaginePost.debugReplyShapeFile` set, every send appends one JSON line to that file: the reply's key structure, with each value replaced by its type. It never records a value, prompt, URL or id, and a key that looks like an id becomes `<key>`. Use it for one real send, correct `wire_provisional.go` and `fakegrok/wire_provisional.go` from it, then turn it off.
- **Import pins only what the account can read** (`import.go`, T6). `POST /posts/import` replaces a hand-written `cz_imagine_post_owner` row. A post already pinned for the key is returned unchanged with no call. A post pinned for another key answers `post_not_found`. Otherwise P3 tries the key's eligible accounts (the create candidates, pinned-only accounts included, since they serve the posts that live on them), in account-id order, or only the labelled one when `account_label` is given. Each try is one free `GET …/responses`. The first that succeeds gets the pin, which is written once, as on create. A 404 or 403 means "not on this account" (provisional: how grok.com answers another account's conversation is unconfirmed). A retryable failure (429, 5xx) is answered as it is, so a retry can still find the post. At most `MaxImportReads` (4) reads. The owner hash is filled on the first item read, as for a create whose read failed.
- **Post-context check after every send**: every conversation id the stream names, and the new asset's `currentConversationId`, must be the post's.
- **Logging and storage** (sec-rowan B5):
  - Logs carry the op, the code, the account label and an 8-hex keyed reference of the conversation id.
  - Logs never carry prompts, `message` or `mediaGenInput`, URLs, `ownerUserId`, titles, tokens, cookies or statsig values. In Statsig `url` mode the transport signs through its own `czApplySignedStatsig`, whose logs name the endpoint with ids replaced (`/rest/assets/{id}`); upstream's signer logs, which print the path, are never reached from P3.
  - The configured `url`-mode signer receives the full request URL, ids included, as it does for every upstream call: it must be an internal service.
  - The `cz_imagine_call` audit rows keep a body hash, never a body, and are pruned after 24 h (`audit.retentionDays: 1`; startup warns if the deployment sets another value).
- **Segments**: `cachedOnly: true` only, at most 512 objects, and `maskUrl` is never read.

## Decisions taken here

Each one is a default, not a new design.

1. **Text-only create is refused** (`capability_unsupported`). Grok makes text-only images over the imagine WebSocket, which isn't on the allow-list, and its in-post behaviour is unknown. A character's post starts from an uploaded anchor.
2. **The account for a new post** follows ADR-0002 § 1's deterministic ranking. `gateway.Service.CZImagineNewCandidates` lists, without leasing or calling grok.com, every web account in the key's scope that is enabled, signed in, serves both image edit and 720p video, and isn't cooling or out of quota, with the remaining quota grok2api holds. P3 drops the pinned-only accounts and ranks the rest: fewest open pinned posts, then most remaining image-edit quota, then most remaining 720p video quota (unknown after any known count), then the lowest account id. It then walks that order: an account with a send in flight, or one the pool refuses when leased, is skipped for the next; a leased account must pass a free `quota_info` read confirming 15 s at 720p. At most 4 quota reads per create. If none passes, the create is `account_unavailable` (`account_busy` if every eligible account was busy), and nothing is uploaded or sent.

   **Pinned-only accounts**: `cz.imaginePost.pinnedOnlyAccountIds` (grok2api account ids, default empty) serve only the posts already pinned to them and are never chosen for a new post (the owner's main account, if O7 becomes yes; under O7's default that account isn't in the pool).
3. **`maxVideoDurationSeconds` absent means `spec_unconfirmable`** (fail closed), and with decision 2 it also means no account can create a post. Where `quota_info` carries the field is provisional: if the spike shows the answer never carries it, change `parseQuota` and the fake together.
4. **The parent `head`** is the newest response by `createTime`, because the list doesn't name the container's `lastResponseId` (provisional).
5. **`skipCancelCurrentInflightRequests: true`** in in-post sends, so a send never cancels another response (provisional, U1).
6. **Account labels and owner hashes are keyed by `secrets.credentialEncryptionKey`.** Rotating that key changes every label and hash, so every existing post then answers `account_label_mismatch`. Treat a key rotation as breaking for posts.
7. **`context_verified: false` on success** means the stream named no conversation and the free asset re-read failed. The layer must treat such an item as unverified and verify it with a later free read.

## Upstream files P3 changes, and why

| File | Lines | Why |
|---|---|---|
| `internal/application/gateway/video.go` | +11 | the pin fix: when attempt 0's `AcquirePinned` fails for the job's recorded account, the job ends `upstream_pinned_account_unavailable` instead of being excluded and reselected (ADR-0002 § 1, test F4) |
| `internal/transport/http/server.go` | +3 | one import, one `Dependencies` field, one `registerCZImaginePost` call |
| `internal/app/application.go` | +3 −1 | build P3 after the gateway, reload its config, pass it to the router |
| `internal/infra/config/config.go` | +1 | the `cz` section (strict YAML needs the field) |

**What the `video.go` fix doesn't cover (open question for arch-theo and sec-rowan):** on upstream's `/v1/videos` path, a create-stage failure on attempt 0 (one that grok2api treats as not sent) is still retried on another account, as upstream designs it. Extending the pin to every attempt makes upstream's own test `TestVideoWebForbiddenRetriesPinnedAccountOnceThenFailsOver` (`application/gateway/video_test.go:786`) fail, so P3 doesn't do it: committed tests aren't edited to pass. P3's post path never uses that loop. Every post send goes through `imaginepost`, which has no reselect at all.

**New files in upstream packages** never conflict on a pull:

- `infra/provider/web/cz_imagine_transport.go`: it sits in package `web` because the signer and header builders are unexported;
- `application/gateway/cz_imaginepost_pool.go`: exact-account leasing;
- `infra/persistence/relational/cz_gorm.go`: the GORM handle;
- `infra/config/cz.go`;
- `app/cz_imaginepost.go`;
- `transport/http/cz_imaginepost.go`;
- their tests.

Rebase budget (F8): P3's hunks in upstream files total 19 changed lines, under the 60-line budget. Check it with `git diff cz-e2fd7ab-p2..codizelabs --stat -- . ':!backend/internal/cz' ':!*cz_*'`.

## Tests

`go test ./internal/cz/... ./internal/application/gateway/ ./internal/infra/provider/web/ ./internal/transport/http/ ./internal/infra/config/`

| Row | Where |
|---|---|
| F1 allow-list | `security_test.go` (table plus zero calls) |
| F2 statsig | `web/cz_imagine_transport_test.go` (every `/rest/` call signed; an invalid value makes zero calls) and `security_test.go` (service level) |
| F3 pin | `security_test.go` (two accounts) and `gateway/cz_imaginepost_pool_test.go` |
| F4 `video.go` fix | `gateway/cz_video_pin_test.go` |
| F5 `job_id` | `security_test.go` (also across a restart) |
| F6 audit | `security_test.go` |
| F7 like | `security_test.go`, `http/handler_test.go`, `config/cz_test.go` |
| F8 | the command above, by hand on each upstream pull |
| P4 account ranking, pinned-only, sign before upload, owner hash fill | `p4_test.go` |
| T5 re-read and reply-shape capture | `t5_reply_test.go` |
| T6 import | `t6_import_test.go` and `http/import_test.go` |

Functional behaviour is in `service_test.go`; the fake itself in `fakegrok/fakegrok_test.go`.
