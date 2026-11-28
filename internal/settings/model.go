package settings

import "strings"

// Capability is a verified capability claim. CapabilityUnknown is the zero value
// and means nobody verified it, which is weaker than CapabilityUnsupported.
type Capability string

const (
	CapabilityUnknown     Capability = ""
	CapabilitySupported   Capability = "supported"
	CapabilityUnsupported Capability = "unsupported"
)

// Supported reports whether the capability is explicitly supported.
func (c Capability) Supported() bool { return c == CapabilitySupported }

// Capabilities are what a model was verified to support. Each field is
// tri-state and defaults to CapabilityUnknown, so a model that never claimed a
// capability is not treated as supporting it.
type Capabilities struct {
	Streaming        Capability
	ToolCalling      Capability
	StructuredOutput Capability
}

// Model is one catalogue entry: the model an alias resolves to and what was
// verified about it. Nothing in the entry is discovered at runtime — the model
// id comes from the configuration and from the reference measured in
// docs/litellm-provider-compatibility.md. Provider and alias are not duplicated
// here: the provider is the prefix of Model and the alias is the map key.
type Model struct {
	// Model is LiteLLM's model_name, exactly "<provider>/<model>" as the proxy
	// lists and resolves it.
	Model string
	// Enabled reports whether routing may use this entry. Absent means enabled,
	// so listing a model is enough to use it.
	Enabled *bool
	// ContextLimit is the verified input context window in tokens. Zero means
	// unverified; a request constraint never relies on it then.
	ContextLimit int
	Capabilities Capabilities
	// Tasks are the task classes this model is suitable for, for example
	// "coding" or "reasoning".
	Tasks []string
	// Unavailable is an optional recurring weekly schedule during which routing
	// must not select this model, in the model's own local time. Absent or empty
	// means no time-based restriction.
	Unavailable *Unavailable
}

// IsEnabled reports whether routing may use this entry. Listing a model is
// enough to use it; the field only exists to switch one off.
func (m Model) IsEnabled() bool { return m.Enabled == nil || *m.Enabled }

// Provider returns the provider prefix of the model id. It is derived rather
// than stored, so the entry cannot name two different providers at once.
func (m Model) Provider() string {
	p, _, _ := strings.Cut(m.Model, "/")
	return p
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
