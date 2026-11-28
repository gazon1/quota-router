package config

import "github.com/gazon1/quota-router/internal/settings"

// Catalogue returns the runtime catalogue view of this config: the configured
// entries and policy converted to the tag-free types routing consumes. It is
// the single conversion point, so nothing downstream reads a koanf tag.
func (c Config) Catalogue() settings.Catalogue {
	models := make(map[string]settings.Model, len(c.LiteLLM.ModelAliases))
	for alias, m := range c.LiteLLM.ModelAliases {
		models[alias] = m.runtime()
	}
	return settings.Catalogue{ModelAliases: models, Routing: c.Routing.runtime()}
}

// Candidates returns, sorted, the aliases routing may use for a request with
// these requirements.
func (c Config) Candidates(r Requirements) []string {
	return c.Catalogue().Candidates(r)
}
