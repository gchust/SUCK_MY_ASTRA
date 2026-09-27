# Detection safety changes

Scope: probe sample integrity, account attribution, uncertainty, and SSE parsing.
This change does not deploy services, change production routing configuration,
retrain the reference bank, or run paid upstream probes.

## Main-based transport choice

This patch is based on main commit `eb215b0f8819e63bb854db92a97e324049fa8823`.
Plugin-to-FC minting stays on HTTP POST. `cloud_mint.transport` still selects
SSE or WebSocket for the separate FC-to-upstream hop. Business WebSocket
connections and their existing relay/continuation handling remain available.
The removed `fc_ws_tunnel` switch and its dedicated client are not restored.

The control-plane regression test checks HTTP POST, the selected upstream
transport header, absence of a WebSocket Upgrade, and successful ticket validation
for both upstream transports, using only a loopback fixture.

The existing background pool-fill switch semantics are unchanged. Applying this
patch is not a command to stop pool filling, clear tickets, or change live settings.

## Probe outcomes

- `fingerprint_match`: agreement with the reference classifier after its gates.
- `fingerprint_mismatch`: disagreement with the reference classifier after its gates.
- `uncertain`: insufficient/inconsistent samples, an unknown requested model,
  ambiguous scores, or missing/unmet independently validated decision thresholds.
- `probe_failed`: no admissible completed outputs or a probe execution failure.

Neither agreement nor disagreement proves backend model identity or capability.
The legacy `fingerprint.match` is true only for a gated `fingerprint_match`.
`candidate_match` separately records the ungated top-candidate equality.
Probabilities remain closed-set relative classifier outputs, not guarantees of
online identity or out-of-distribution detection.

The existing reference bank has temperature calibration but no rejection policy.
It therefore produces exploratory predictions with an `uncertain` verdict by
default. No 95% or other production threshold has been invented. A future
`decision_policy` must identify independent validation in `validation_id` and
provide `min_valid_outputs`, `min_confidence`, and `min_margin`. Reference-data
cross-validation alone does not establish these rejection thresholds. The bank
JSON and its fitted coefficients are unchanged by this patch.

Only responses with a valid `response.created` identity followed by a correlated
`response.completed` are successful. EOF, close, malformed JSON, incomplete,
failed, and mixed response identities are failures. A completed sample must also
contain exactly the requested number of valid integers. Failed or partial text
is retained only as diagnostic counts, never classified.

## Compatibility and rollout

The FC grade response adds `completed` and `response_id`. The updated plugin
requires both and fails closed against an older relay that cannot supply them.
An authorized deployment must update/test the relay contract before enabling FC
grade probes on the updated plugin. Ordinary minting is not changed by this
contract addition. No deployment is performed as part of this source patch.

Client-path probes use CPA automatic routing. They reject account selection
instead of labelling automatically routed responses as a selected account.
Selected credential probes remain available on FC/bridge paths. No unsupported
account-pinning header is introduced.

The passive SSE observer buffers per request, parses complete events, requires
`response.created` with nonempty `response.id` and `response.model`, and ignores
unrelated nested model fields. It supports LF/CRLF/CR and multiline data. Scans
have a 64 KiB cumulative byte budget, the existing request TTL, and the existing
maximum pending-request count. Exhausted/invalid scans yield no downgrade claim.

## Offline verification

Go regression tests cover completion, count integrity, selection rejection,
uncertainty gates, fragmented SSE, attribution, byte limits, and expiration.
`relay/modeltrace_safety.test.js` uses only in-memory fixture events. Other relay
tests use loopback-only fake upstreams. No real keys, cookies, accounts, or upstream
service calls are needed.

Gateway ranking statistics, global probe concurrency/cancellation budgets, and
independent classifier calibration remain outside this first patch.

## Previous branch verification: 2026-09-26

Base: `fc-ws-bridge`, commit `54f44e34db42a733c38a7dfef2cd68095c214673`.
These results precede the migration to main and do not substitute for testing
the main-based patch. Nothing was deployed as part of that verification.

Completed through SSH in isolated containers on an existing build host:

- `go test -count=1 -p=1 ./...`: PASS on the final Go source.
- `go test -race -count=1 -p=1 ./...`: PASS on the final Go source.
- `node --test relay/index.test.js relay/modeltrace_safety.test.js tests/ui/modeltrace-safety.test.cjs`:
  175 passed, 0 failed, 0 skipped.
- Local `git diff --check`: PASS.

Only cached Go/Node images and cached module dependencies were used. Containers
ran with external networking disabled and restricted CPU/memory/capabilities.
Relay network tests used loopback fixtures; UI regressions used mocked inputs and
requests. No paid probe, production credential, dependency download, or service
restart was needed. Initial test-container ownership and an obsolete Go import
were corrected before the successful final runs.

Execution logs are retained outside the published source tree. No private
infrastructure addresses, filesystem locations, or credentials are needed to
reproduce the offline checks.

Not verified: real-browser visual layout, production plugin loading, live FC/CPA
integration, reference-label authenticity, or independent classifier accuracy.
These offline passes are implementation evidence, not a model-quality claim.

## Main-based verification: 2026-09-27

Fresh checks completed against main `eb215b0f8819e63bb854db92a97e324049fa8823`
plus this patch, using cached containers with external networking disabled:

- `go test -count=1 -p=1 -timeout=120s ./...`: PASS.
- `go test -race -count=1 -p=1 -timeout=120s ./...`: PASS.
- `node --test relay/index.test.js relay/modeltrace_safety.test.js tests/ui/modeltrace-safety.test.cjs tests/ui/observed-state.test.mjs`:
  176 passed, 0 failed, 0 skipped, including upstream observed-state checks.
- The HTTP control-plane contract passes for both SSE and WebSocket upstream
  selection; business WebSocket functionality remains in the main-based tree.
- No `fc_ws_tunnel`, `FCWSTunnel`, or `mintViaFCTunnel` implementation is present.

The first Node launcher returned no usable result. A separate read-only check
confirmed that neither its container nor log existed before it was launched
again. This was not a failed test assertion.

These are offline implementation checks. Browser visual layout, production ABI
loading, and live FC/CPA behavior have not been validated. No production service,
pool-fill configuration, deployment, or paid upstream probe was changed.
