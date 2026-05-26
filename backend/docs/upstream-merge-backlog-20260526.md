# Upstream Merge Backlog - 2026-05-26

Scope: items observed from Wei-Shaw/sub2api open PRs after the v0.3 HTTP/2 and
stream-error fixes. This file records work that is useful but should not be
mixed into the current production push.

## Already Covered In `release/sub2api-v0.3`

- OpenAI HTTP/2 transport stability:
  - configurable OpenAI response-header timeout
  - HTTP/2 compatibility fallback to HTTP/1.1 for proxy incompatibility errors
  - `response.failed` terminal event after started Responses streams
  - JSON-marshaled `response.failed` payload
  - WS continuation recognition for Codex tool outputs
- Existing local adaptations already cover earlier targeted PR work:
  - service-tier filtering / `fast` to `priority` normalization
  - file URL support
  - OAuth `store=false` item-reference stripping
  - SSE terminal event recognition
  - image billing tier mapping

## P0 / P1 Candidates For The Next Merge Branch

### #2787 Codex/OpenAI hyphen session headers

Why useful: may improve Codex/OpenAI client session continuity when clients send
hyphenated session headers instead of only the currently recognized names.

Merge strategy: inspect exact header names and map them into the existing
session/turn-state key derivation without forwarding new private headers to
upstream unless intentionally needed.

Risk: low to medium. Wrong header precedence can pollute turn-state cache keys.

### #2755 Drop invalid OpenAI placeholder tools before forwarding

Why useful: prevents invalid placeholder tools from reaching OpenAI upstream and
turning into avoidable 400s.

Merge strategy: extract only the validation/drop rule. Keep our Claude mimic
tool mapping and gcr/cc-api tool-name disguise rules unchanged.

Risk: medium. Dropping a real client tool by mistake would break tool workflows.

### #2729 OpenAI messages `count_tokens` dispatch

Why useful: improves Anthropic-compatible `count_tokens` behavior for OpenAI
backed accounts.

Merge strategy: port endpoint dispatch and tests. Verify it does not call the
expensive generation path and does not create fake cache usage.

Risk: low to medium. Billing/token accounting paths need targeted tests.

### #2725 Non-stream Responses output ids / annotations

Why useful: OpenAI Responses compatibility; may reduce shape differences for
non-stream clients.

Merge strategy: port only the response-shape completion fields and add recorder
tests for non-stream `/v1/responses`.

Risk: low. Main risk is changing fields expected by our apicompat converters.

### #2697 Ignore client cancel in concurrency accounting

Why useful: we have seen NewAPI `client_gone/context canceled` while sub2api
continues draining for usage. This PR may reduce false concurrency pressure.

Merge strategy: extract the accounting condition only. Keep the current drain
for billing behavior.

Risk: medium. A wrong extraction can undercount real active upstream requests.

### #2603 Sanitize Claude thinking history before OpenAI Responses forwarding

Why useful: protects OpenAI-backed paths from invalid Claude thinking /
redacted_thinking history blocks that can produce upstream rejects.

Merge strategy: adapt narrowly for OpenAI-forwarded history only. Do not strip
client-visible thinking from Anthropic-shaped responses, and do not break gcr
signature splicing semantics.

Risk: high. This touches thinking/signature semantics and can regress cctest
signature or Claude mimic behavior.

### #2286 Finalize partial Anthropic streams on missing terminal events

Why useful: relevant to stream interruption cases; can avoid clients seeing a
silent EOF when the upstream ended without a proper terminal event.

Merge strategy: compare with our existing `response.failed` handling and
Anthropic apicompat stream finalization. Port only the terminal synthesis logic
that is still missing.

Risk: medium. Must not double-emit terminal events.

## Useful But Separate Product Work

### #2735 Content moderation per-category thresholds

Why useful: admin-controlled risk thresholds are product-useful.

Merge strategy: separate feature branch. Check whether our current code still
has the same moderation module boundaries before cherry-pick.

Risk: medium. Schema/UI/API changes.

### #2715 Per-account Codex CLI version pinning

Why useful: lets operations pin Codex client versions per account, useful for
compatibility and disguise testing.

Merge strategy: feature branch with UI + backend + config tests.

Risk: medium. Wrong default can fragment fingerprints.

### #2711 DB pool minimum lifetime / goroutine leak mitigation

Why useful: operational reliability under long-running production load.

Merge strategy: small isolated backend patch plus DB pool regression test.

Risk: low.

### #2686 Record upstream error body in ops logs

Why useful: improves debugging of upstream rejects.

Merge strategy: only store bounded, redacted snippets. Never expose raw upstream
error bodies to end clients.

Risk: medium due to sensitive data/log volume.

### #2635 Log redaction for payment and cloud secrets

Why useful: security hardening.

Merge strategy: safe to port after verifying it does not redact fields needed
for current diagnostics.

Risk: low to medium.

### #2326 / #2327 OpenAI quota scheduling controls

Why useful: better account selection by remaining quota.

Merge strategy: larger scheduling feature branch. Needs migration, UI, scheduler
tests, and compatibility with local account grouping.

Risk: high.

## Keep Deferred Unless Business Decides Otherwise

### #2255 API-key Chat Completions account type

Large new account type and bridge. Useful only if we explicitly want upstreams
that speak Chat Completions instead of Responses. Requires a separate design.

### #2257 Empty instructions fallback

Can alter behavior probes and model identity behavior. Do not merge globally
until cctest and normal client behavior are A/B tested.

### #2373 Service-tier injection policy

We currently filter client-supplied service tier to avoid cost and disguise
leaks. Any automatic injection changes cost semantics and must be a business
decision, not a blind upstream merge.

### #2519 Large refactor

Useful only as a reference. Do not hard-merge into the current fork because it
touches many files and can bury disguise regressions.
