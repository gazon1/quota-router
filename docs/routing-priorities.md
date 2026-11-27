# Routing priorities

Issue: [gazon1/quota-router#53](https://github.com/gazon1/quota-router/issues/53)
Code: [internal/config/config.go](../internal/config/config.go), [validate.go](../internal/config/validate.go)
Config: [config.example.yaml](../config.example.yaml)

`routing.providers` defines the static operator preference order. It is
configuration only: #17 applies it after hard constraints and viable session
affinity, and it never bypasses capability, health, quota, or safety checks.

## Semantics

- **Lower number = higher preference.** Priority values are positive integers.
- **Provider priority** ranks providers. Provider keys are provider names as they
  appear in model ids (`<provider>/<model>`).
- **Model priority** ranks catalogue aliases within one provider. It is scoped to
  that provider and is never compared across providers.
- **Complete when configured.** If `routing.providers` is set, every enabled
  catalogue alias must be listed under its provider, every listed alias must exist
  and be enabled, and provider/model priorities must not be duplicated within their
  scope.
- **Disabled aliases stay out.** An alias with `enabled: false` is not routable and
  must not appear in the routing order; it can be re-enabled only together with a
  routing entry.
- **No secrets.** Credentials stay in `providers.<name>.api_key` / env / secret
  files per #7.

## Example

```yaml
litellm:
  model_aliases:
    fast:
      model: minimax/MiniMax-M2.7
    free-coding:
      model: commandcode/stealth/glyph-cluster:free
      context_limit: 256000
      capabilities:
        streaming: supported
        tool_calling: supported
        structured_output: unsupported
      tasks: [coding]
    retired:
      model: commandcode/deepseek-v4.1-flash
      enabled: false

routing:
  low_quota_threshold: 0.1
  providers:
    commandcode:
      priority: 1          # preferred provider
      models:
        free-coding: 1     # preferred within commandcode
    minimax:
      priority: 2
      models:
        fast: 1
```

With this config, #17 considers enabled candidates in this static order after hard
constraints and viable session affinity: `commandcode/free-coding`, then
`minimax/fast`. `retired` is disabled, so it is excluded before preference
ordering and does not need a routing entry.

## Environment overrides

Environment variables override file values, same as other config (#7):

```sh
QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__PRIORITY=1
QUOTA_ROUTER_ROUTING__PROVIDERS__COMMANDCODE__MODELS__FREE-CODING=1
QUOTA_ROUTER_ROUTING__PROVIDERS__MINIMAX__PRIORITY=2
QUOTA_ROUTER_ROUTING__PROVIDERS__MINIMAX__MODELS__FAST=1
```

`__` separates nesting levels. For map-key segments such as provider names and
catalogue aliases, literal hyphens are supported in the environment variable name
(for example `FREE-CODING` for alias `free-coding`). In shells where `export`
does not allow hyphens in identifiers, use underscores (`FREE_CODING`); on load,
an underscore form is mapped to the hyphenated catalogue alias only when that
hyphenated alias exists and the underscore form is not itself a configured
catalogue alias. Aliases that legitimately contain underscores are not remapped.

## Validation

Startup rejects:

- unknown provider in `routing.providers` with no enabled catalogue aliases;
- missing provider for an enabled catalogue alias;
- unknown alias, disabled alias, or alias listed under the wrong provider;
- missing enabled alias when routing is configured;
- duplicate provider priorities;
- duplicate model priorities within the same provider;
- non-positive provider or model priority.

## Boundaries

- #17 owns candidate selection, affinity, hard constraints, cooldown/quota/health
  exclusion, deterministic fallback, and decision explanations.
- #16/#29 request-level preferred provider/model and manual overrides remain
  distinct from this static default ordering.
- Measured performance must not silently override explicit configured priority.
