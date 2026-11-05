# Optional Codex checkpoint handoff

Codex can send a native OpenAI compaction item after the user switches to a
model served by another provider. When that checkpoint is the only copy of
the earlier conversation, translating it away loses that earlier context.

`codexCheckpointModel` opts into recovering an **untracked OpenAI checkpoint**
through the caller's ChatGPT sign-in before routing the request to the new
model. It is off by default. Existing routing, native compaction and writer
ownership behavior stay unchanged while it is off.

## Enable or disable

Add `codexCheckpointModel` to the existing `settings.json`, with a bare
OpenAI model id the caller's ChatGPT account can use:

```json
{"codexCheckpointModel": "gpt-6-luna"}
```

Remove that property or set it to an empty string to disable recovery.
This first version is a config-file opt-in; it has no Settings-page control
or `magpie settings` command. Saving unrelated settings preserves it.

This is separate from a routing group's compaction rule: those rules choose
who writes a new summary. This option recovers a checkpoint that OpenAI has
already sealed, which a different provider cannot read.

This first recovery source is ChatGPT/OpenAI; destination providers continue
to use Magpie's existing integrations. Future recovery sources need their own
authenticated reader for that provider's sealed checkpoints, producing a
portable summary with the same failure and credential boundaries. There is
no generic recovery-plugin API in this version.

## Behavior and boundaries

- Only requests to `/backend-api/codex/responses` for a known Magpie model
  or group use this option. Codex's own unqualified model ids, other agents'
  endpoints, and unknown destinations retain their previous routing.
- Recovery uses the incoming `Authorization` and `ChatGPT-Account-ID`,
  never a saved account, a provider API key or another pool account.
  Missing credentials, API-key callers, gateway-key callers and explicit
  `X-Magpie-Account` overrides are refused rather than borrowing credentials.
- A checkpoint whose native writer Magpie recorded keeps the existing
  ownership guard. A destination configured for native compaction keeps
  native replay. This first version does not convert those checkpoints.
- The option identifies otherwise untracked checkpoints as OpenAI's. It
  accepts the native sealed format, not a different gateway's private
  checkpoint encoding. It does not import Fleet-specific summaries.
- Only the checkpoint and a fixed recovery instruction go to OpenAI. The
  destination's tools, current user message and provider credential do not.
  Redirects are refused. The source call has a one-minute bound.
- A complete, nonempty text summary replaces the checkpoint as historical
  context in the outgoing request. Tool-call identifiers and the rest of
  the inline history remain intact. An upstream storage-chain reference
  (`previous_response_id`) is removed after conversion.
- On an authentication error, quota error, network error, failed or incomplete
  stream, empty/tool-calling summary, or oversized reply, the gateway returns
  an error before contacting the destination. The original request and the
  agent's stored history are not rewritten. Error responses do not repeat
  the source's body, ciphertext or credentials.

Recovery is lossy and spends the caller's ChatGPT allowance. It is logged
as a Magpie `compaction` usage row, including reported token counts. Successful
summaries are kept only in this server's memory, keyed by a digest of the
caller token, account, recovery model and ciphertext. The cache holds at
most 64 summaries of at most 128 KiB each; token/account/model changes and
eviction cause another recovery request. Concurrent first uses may each
recover the checkpoint; no network operation holds the cache lock.

The destination receives the recovered summary through the usual gateway
translation and masking path. No recovery summary or credential is written
to a checkpoint cache on disk. Other independently enabled request-archive
features retain their normal behavior.

## Verification

`internal/gateway/codex_checkpoint_handoff_test.go` exercises the actual
server front, with loopback OpenAI and destination fixtures. It checks the
checkpoint-only case, credential separation, no current context sent to the
source, failure behavior, cache identity/eviction, redirects, cancellation,
default-off behavior, private formats and native writer ownership.

`internal/settingsapi/codex_checkpoint_test.go` checks opt-in, validation,
preservation across unrelated settings saves and disabling recovery.

```sh
go test -tags nogui ./internal/gateway ./internal/settingsapi \
  -run 'TestCodexCheckpoint' -count=1
go test -race -tags nogui ./internal/gateway -run '^TestCodexCheckpointHandoff' -count=20
```
