package config

import "github.com/gazon1/quota-router/internal/settings"

// Catalogue returns the runtime catalogue view of this config: the configured
// entries and policy converted to the tag-free types routing consumes. It is
// the single conversion point, so nothing downstream reads a koanf tag.
//
// The view shares the config's immutable values — the Enabled pointer, Tasks
// and Days slices, and Models maps are not copied. That is deliberate: the
// loader runs once at startup and the result is never mutated afterwards, so a
// deep copy would buy nothing and cost allocations on every call. Treat both
// Config and its Catalogue as read-only once Load returns.
func (c Config) Catalogue() settings.Catalogue {
	models := make(map[string]settings.Model, len(c.LiteLLM.ModelAliases))
	for alias, m := range c.LiteLLM.ModelAliases {
		models[alias] = m.runtime()
	}
	return settings.Catalogue{ModelAliases: models, Routing: c.Routing.runtime()}
}
