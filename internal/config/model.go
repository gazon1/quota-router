package config

import "github.com/gazon1/quota-router/internal/settings"

// The types in this file are the configuration shape of the model catalogue:
// they carry the koanf field names and validation rules the loader needs.
// internal/settings holds the runtime shape, which carries neither. Catalogue
// converts one into the other, so nothing outside loading sees a koanf tag.

// Capabilities are what a model was verified to support.
type Capabilities struct {
	Streaming        Capability `koanf:"streaming" validate:"omitempty,oneof=unknown supported unsupported"`
	ToolCalling      Capability `koanf:"tool_calling" validate:"omitempty,oneof=unknown supported unsupported"`
	StructuredOutput Capability `koanf:"structured_output" validate:"omitempty,oneof=unknown supported unsupported"`
}

// Model is one configured catalogue entry: the model an alias resolves to and
// what was verified about it.
type Model struct {
	// Model is LiteLLM's model_name, exactly "<provider>/<model>".
	Model string `koanf:"model" validate:"required,model_id"`
	// Enabled reports whether routing may use this entry. Absent means enabled.
	Enabled *bool `koanf:"enabled"`
	// ContextLimit is the verified input context window in tokens. Zero means
	// unverified.
	ContextLimit int          `koanf:"context_limit" validate:"gte=0"`
	Capabilities Capabilities `koanf:"capabilities"`
	// Tasks are the task classes this model is suitable for.
	Tasks []string `koanf:"tasks" validate:"dive,required"`
	// Unavailable is an optional recurring weekly schedule during which routing
	// must not select this model. Absent or empty means no time-based restriction.
	Unavailable *Unavailable `koanf:"unavailable" validate:"omitempty"`
}

// UnmarshalText accepts the short form the catalogue shipped with
// (`fast: minimax/MiniMax-M2.7`) as a model id with everything else unknown, so
// widening the entry did not break an existing configuration file.
func (m *Model) UnmarshalText(b []byte) error {
	m.Model = string(b)
	return nil
}

// Unavailable is the configurable recurring local-time schedule during which
// routing must not select a model.
type Unavailable struct {
	// Timezone is the IANA zone the windows are written in.
	Timezone string `koanf:"timezone"`
	// Windows are recurring weekly intervals.
	Windows []UnavailableWindow `koanf:"windows" validate:"dive"`
}

// UnavailableWindow is one recurring weekly interval in local wall-clock time.
// Days name the weekday the window starts on, Start is inclusive and End is
// exclusive; Start after End is an overnight window.
type UnavailableWindow struct {
	Days  []string `koanf:"days" validate:"required,dive,weekday"`
	Start string   `koanf:"start" validate:"required,clock_hhmm"`
	End   string   `koanf:"end" validate:"required,clock_hhmm"`
}

// runtime converts the entry to the runtime shape routing consumes.
func (m Model) runtime() settings.Model {
	out := settings.Model{
		Model:        m.Model,
		Enabled:      m.Enabled,
		ContextLimit: m.ContextLimit,
		Capabilities: settings.Capabilities{
			Streaming:        m.Capabilities.Streaming,
			ToolCalling:      m.Capabilities.ToolCalling,
			StructuredOutput: m.Capabilities.StructuredOutput,
		},
		Tasks: m.Tasks,
	}
	if m.Unavailable != nil {
		u := m.Unavailable.runtime()
		out.Unavailable = &u
	}
	return out
}

func (u Unavailable) runtime() settings.Unavailable {
	windows := make([]settings.UnavailableWindow, len(u.Windows))
	for i, w := range u.Windows {
		windows[i] = settings.UnavailableWindow{Days: w.Days, Start: w.Start, End: w.End}
	}
	return settings.Unavailable{Timezone: u.Timezone, Windows: windows}
}
