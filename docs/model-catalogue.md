# Model catalogue and capability registry

Issue: [gazon1/quota-router#15](https://github.com/gazon1/quota-router/issues/15)
Code: [internal/config/config.go](../internal/config/config.go), [catalogue.go](../internal/config/catalogue.go), [verify.go](../internal/config/verify.go)
Evidence: [compatibility matrix](./litellm-provider-compatibility.md), [API verification](./minimax-api-verification.md)

The catalogue is `litellm.model_aliases`: a routing alias mapped to the entry
LiteLLM serves under that name. It is the only place model ids come from — the
controller never calls a provider's `/models` to discover or invent one. This
extends the configuration from #7 rather than adding a second source, so
precedence, secret handling and validation errors are the ones already documented
in [README.md](../README.md).

```yaml
litellm:
  model_aliases:
    free-coding:
      model: commandcode/stealth/glyph-cluster:free
      context_limit: 256000
      capabilities:
        streaming: supported
        tool_calling: supported
        structured_output: unsupported
      tasks: [coding]
    fast: minimax/MiniMax-M2.7   # short form: the id and nothing claimed
```

| Field | Meaning |
|---|---|
| `model` | LiteLLM's `model_name`, exactly `<provider>/<model>`. Provider = its prefix, alias = the map key, so neither is repeated. |
| `enabled` | Default `true`. `false` keeps the entry configured but out of routing. |
| `context_limit` | Verified input window in tokens. `0` = unverified; a request constraint never relies on it then. |
| `capabilities.streaming`, `.tool_calling`, `.structured_output` | `supported`, `unsupported`, or absent = unverified. |
| `tasks` | Task classes this model suits, for example `coding`. |

Environments cannot hold a YAML sequence, so `...TASKS=coding,reasoning` is split
on commas. The variable names are in [README.md](../README.md#environment-variables).

## Unknown capabilities are conservative

A capability is tri-state, and only an explicit `supported` satisfies a request:

| Catalogue says | `Requirements{Streaming: true}` |
|---|---|
| `supported` | matches |
| `unsupported` | excluded |
| absent (unverified) | **excluded** |

That is the whole point of a separate `unknown` state: a model nobody measured
must not silently answer a request that needs the capability. So an entry that
claims nothing is eligible only for a request that requires nothing.

`Config.Candidates(Requirements)` applies this together with `enabled` and
returns aliases in sorted order, so identical configuration and requirements
produce the same candidate list for #17 to rank.

```go
cfg.Candidates(config.Requirements{
    ToolCalling: true,
    Tasks:       []string{"coding"},
})
```

## Verification against LiteLLM

Startup checks every configured id against the model list the proxy itself serves
(`GET /v1/models`), because an alias LiteLLM does not know fails at request time
otherwise. Set `litellm.api_key` (the proxy master key) to enable it; unset skips
the check.

The two failures are deliberately distinct, because they call for different
actions:

| Outcome | Meaning | Startup |
|---|---|---|
| every id served | catalogue matches the proxy | starts |
| `ErrCatalogueUnavailable` | proxy down, refused, or answered with something that is not a model list | **warns and starts** |
| any other error | proxy answered, and a configured id is not in its list | **exits 1** |

A mismatch is a configuration fault and cannot work once traffic arrives, so it is
fatal. An unreachable proxy is not: [compose](../deploy/compose.yaml) deliberately
has no dependency edge between the two containers, so either may start first, and
a controller that refuses to start because a sidecar is briefly down would add
exactly the dependency that design avoids. The warning names the problem, and the
next restart verifies again.

The check sends the master key as `Authorization: Bearer`, reuses
`polling.request_timeout` as its bound, caps the answer at 1 MiB and never echoes
a response body — an error body can carry a credential.

## What is claimed here, and why it is minimal

Capabilities in [config.example.yaml](../config.example.yaml) are what
[the compatibility matrix](./litellm-provider-compatibility.md) measured through
the pinned proxy, not what a provider's documentation advertises:

- Command Code `stealth/glyph-cluster:free`: chat completions, SSE streaming and
  tool calls returned `200`; `structured_output` was never exercised, so it is
  `unsupported` rather than assumed.
- `context_limit` values come from the `max_input_tokens` that `GET /v1/models`
  reported for the same ids.
- MiniMax `MiniMax-M2.7` claims nothing: inference on the measured account was
  blocked by plan exhaustion (`429`/`2056`), so no capability and no context
  window was verified. The documented 1M window belongs to M3.1-Flash/M3, not to
  this id.

So the shipped example is honest rather than optimistic, and the conservative rule
above means an unmeasured model is simply never selected for a job it cannot prove
it does.

## Limitations

- **No runtime discovery.** Neither provider `/models` nor LiteLLM is consulted to
  learn ids; the list is configuration, and verification only rejects entries the
  proxy does not serve.
- **Verification is opt-in.** With `litellm.api_key` unset an unknown alias is
  caught at request time, not at startup.
- **Not wired in the compose stack yet.** The controller container reaches the
  proxy at `http://litellm:4000`, while the default `litellm.base_url` is
  loopback. Until #38 connects the two, the shipped deployment runs with the check
  skipped (no `litellm.api_key`) rather than reporting a permanent false
  "unavailable".
- **No period re-check.** Verification runs at startup only; a health-check
  re-check is a later change.
- **`tasks` are a free vocabulary at the catalogue layer.** Validation rejects an
  empty class but not an unknown one. Agent-supplied routing request task
  classes are defined and validated in
  [docs/routing-request.md](./routing-request.md) (#16); selection policy
  decides whether a class has effect (#17).
- **Static priority is configured separately.** Provider and per-provider model
  ordering lives in `routing.providers` (#53, [routing-priorities.md](./routing-priorities.md));
  selection is #17. This package only answers "which aliases may serve this
  request".
