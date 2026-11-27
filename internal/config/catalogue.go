package config

// Candidates returns, sorted, the aliases routing may use for a request with
// these requirements.
//
// Selection is conservative: a capability counts only when the catalogue says
// "supported", so an unverified or unsupported one excludes the model. A
// requirement the catalogue never confirmed is therefore never answered by a
// guess. Disabled entries are never candidates. An empty Requirements matches
// every enabled model.
func (c Config) Candidates(r Requirements) []string {
	aliases := make([]string, 0, len(c.LiteLLM.ModelAliases))
	for _, alias := range sortedAliases(c.LiteLLM.ModelAliases) {
		if m := c.LiteLLM.ModelAliases[alias]; m.IsEnabled() && r.SatisfiedBy(m) {
			aliases = append(aliases, alias)
		}
	}
	return aliases
}

// Model returns the catalogue entry for an alias.
func (c Config) Model(alias string) (Model, bool) {
	m, ok := c.LiteLLM.ModelAliases[alias]
	return m, ok
}

// Requirements are the capabilities and task classes a request needs.
type Requirements struct {
	Streaming        bool
	ToolCalling      bool
	StructuredOutput bool
	// Tasks are task classes the model must be suitable for; all are required.
	Tasks []string
}

// SatisfiedBy reports whether a model explicitly meets every requirement.
func (r Requirements) SatisfiedBy(m Model) bool {
	for _, req := range []struct {
		want bool
		have Capability
	}{
		{r.Streaming, m.Capabilities.Streaming},
		{r.ToolCalling, m.Capabilities.ToolCalling},
		{r.StructuredOutput, m.Capabilities.StructuredOutput},
	} {
		if req.want && !req.have.Supported() {
			return false
		}
	}
	for _, want := range r.Tasks {
		found := false
		for _, have := range m.Tasks {
			if have == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
