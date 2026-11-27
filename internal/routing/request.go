package routing

import (
	"errors"
	"fmt"
	"strings"
)

// RouteRequestVersionV1 is the first routing request schema version.
const RouteRequestVersionV1 = 1

// Known task classes. A task class is supplied by the agent; the controller
// must not infer it with an extra LLM call.
const (
	TaskRoutine   TaskClass = "routine"
	TaskCoding    TaskClass = "coding"
	TaskReasoning TaskClass = "reasoning"
)

// TaskClass names the kind of work a routing request describes. It is optional:
// include it only when the routing policy has a concrete effect for it. The
// vocabulary is small and explicit; unknown values are rejected at validation.
type TaskClass string

func (t TaskClass) known() bool {
	switch t {
	case TaskRoutine, TaskCoding, TaskReasoning:
		return true
	default:
		return false
	}
}

// RouteRequest is the agent-to-router contract for one routing decision.
// It is an explicit decision boundary, not an instruction to switch models
// after every agent message. The router selects an eligible provider/model for
// this request; the agent decides when to ask.
type RouteRequest struct {
	// Version identifies the request schema version.
	Version int `json:"version"`
	// SessionID identifies the agent session. The router may use it to preserve
	// session affinity where applicable.
	SessionID string `json:"session_id"`
	// Capabilities are hard requirements the selected model/provider must
	// satisfy. A model that cannot satisfy them is ineligible regardless of
	// preferences or overrides. The field is required; an empty object means no
	// capability requirements.
	Capabilities *RequiredCapabilities `json:"capabilities"`
	// TaskClass is an optional agent-supplied task class. The controller must
	// not make an extra LLM call to infer it.
	TaskClass TaskClass `json:"task_class,omitempty"`
	// Preference is an optional request-level provider/model preference. It is
	// not a hard constraint unless represented as one. Operator-configured
	// ordering (#53) is separate.
	Preference *RoutePreference `json:"preference,omitempty"`
	// Override is an optional explicit manual route selection. It is bounded by
	// hard constraints and auditable; it cannot silently bypass capability
	// constraints or select an unconfigured model.
	Override *ManualOverride `json:"override,omitempty"`
}

// RequiredCapabilities are hard requirements for the selected model/provider.
type RequiredCapabilities struct {
	Streaming        bool `json:"streaming,omitempty"`
	ToolCalling      bool `json:"tool_calling,omitempty"`
	StructuredOutput bool `json:"structured_output,omitempty"`
}

// RoutePreference is a request-level preference. It does not override hard
// constraints and is separate from operator-configured provider/model ordering
// (#53).
type RoutePreference struct {
	// Provider prefers a configured provider name.
	Provider string `json:"provider,omitempty"`
	// ModelAlias prefers a configured routing alias.
	ModelAlias string `json:"model_alias,omitempty"`
}

// ManualOverride requests an explicit route selection. It is visible and
// auditable. Hard constraints still apply: the target must be configured,
// enabled, and capability-compatible. An override cannot bypass capability
// constraints, quota/health policy, or select an unconfigured model.
type ManualOverride struct {
	// ModelAlias names the configured routing alias to select explicitly.
	ModelAlias string `json:"model_alias"`
}

// Validate reports malformed, unsupported, and contradictory request values.
// It checks the contract shape, not catalogue membership or runtime eligibility;
// those are selection-time concerns (#17).
func (r RouteRequest) Validate() error {
	var errs []error

	if r.Version != RouteRequestVersionV1 {
		errs = append(errs, fmt.Errorf("version must be %d, got %d", RouteRequestVersionV1, r.Version))
	}
	if strings.TrimSpace(r.SessionID) == "" {
		errs = append(errs, errors.New("session_id must not be empty"))
	}
	if r.Capabilities == nil {
		errs = append(errs, errors.New("capabilities must be present"))
	}

	if r.TaskClass != "" && !r.TaskClass.known() {
		errs = append(errs, fmt.Errorf("task_class %q is not supported; known classes: %s", r.TaskClass, strings.Join(knownTaskClasses(), ", ")))
	}

	if r.Preference != nil {
		if err := r.Preference.validate(); err != nil {
			errs = append(errs, err)
		}
	}
	if r.Override != nil {
		if err := r.Override.validate(); err != nil {
			errs = append(errs, err)
		}
	}

	if r.Preference != nil && r.Override != nil {
		if p := strings.TrimSpace(r.Preference.ModelAlias); p != "" && p != strings.TrimSpace(r.Override.ModelAlias) {
			errs = append(errs, fmt.Errorf("preference.model_alias %q contradicts override.model_alias %q", r.Preference.ModelAlias, r.Override.ModelAlias))
		}
	}

	return errors.Join(errs...)
}

func (p RoutePreference) validate() error {
	var errs []error
	provider := strings.TrimSpace(p.Provider)
	alias := strings.TrimSpace(p.ModelAlias)
	if provider == "" && alias == "" {
		errs = append(errs, errors.New("preference must set provider or model_alias"))
	}
	if p.Provider != "" && provider == "" {
		errs = append(errs, errors.New("preference.provider must not be blank"))
	}
	if p.ModelAlias != "" && alias == "" {
		errs = append(errs, errors.New("preference.model_alias must not be blank"))
	}
	return errors.Join(errs...)
}

func (o ManualOverride) validate() error {
	if strings.TrimSpace(o.ModelAlias) == "" {
		return errors.New("override.model_alias must not be empty")
	}
	return nil
}

func knownTaskClasses() []string {
	return []string{string(TaskRoutine), string(TaskCoding), string(TaskReasoning)}
}
