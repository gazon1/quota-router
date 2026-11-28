package config

import "github.com/gazon1/quota-router/internal/settings"

// Catalogue returns the runtime catalogue view of this config.
func (c Config) Catalogue() settings.Catalogue {
	return settings.Catalogue{ModelAliases: c.LiteLLM.ModelAliases, Routing: c.Routing}
}

// Candidates returns, sorted, the aliases routing may use for a request with
// these requirements.
func (c Config) Candidates(r Requirements) []string {
	return c.Catalogue().Candidates(r)
}

// Model returns the catalogue entry for an alias.
func (c Config) Model(alias string) (Model, bool) {
	return c.Catalogue().Model(alias)
}
