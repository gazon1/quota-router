package settings

import "sort"

// Catalogue is the runtime model catalogue and routing policy consumed by the
// routing selector. It contains no loader, environment, or Koanf concerns.
type Catalogue struct {
	ModelAliases map[string]Model
	Routing      Routing
}

// SortedAliases returns catalogue aliases in a stable order.
func SortedAliases(models map[string]Model) []string {
	aliases := make([]string, 0, len(models))
	for alias := range models {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	return aliases
}

// Candidates returns, sorted, the aliases routing may use for a request with
// these requirements.
//
// Selection is conservative: a capability counts only when the catalogue says
// "supported", so an unverified or unsupported one excludes the model. A
// requirement the catalogue never confirmed is therefore never answered by a
// guess. Disabled entries are never candidates. An empty Requirements matches
// every enabled model.
func (c Catalogue) Candidates(r Requirements) []string {
	aliases := make([]string, 0, len(c.ModelAliases))
	for _, alias := range SortedAliases(c.ModelAliases) {
		if m := c.ModelAliases[alias]; m.IsEnabled() && r.SatisfiedBy(m) {
			aliases = append(aliases, alias)
		}
	}
	return aliases
}

// Model returns the catalogue entry for an alias.
func (c Catalogue) Model(alias string) (Model, bool) {
	m, ok := c.ModelAliases[alias]
	return m, ok
}
