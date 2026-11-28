package config

import "github.com/gazon1/quota-router/internal/settings"

// Temporary compatibility aliases: canonical runtime types live in
// internal/settings. Keep these aliases to avoid avoidable churn while callers
// migrate; do not add new behavior here.
type (
	Secret            = settings.Secret
	Model             = settings.Model
	Capability        = settings.Capability
	Capabilities      = settings.Capabilities
	Requirements      = settings.Requirements
	Unavailable       = settings.Unavailable
	UnavailableWindow = settings.UnavailableWindow
	Routing           = settings.Routing
	ProviderRouting   = settings.ProviderRouting
	Provider          = settings.Provider
)

const (
	CapabilityUnknown     = settings.CapabilityUnknown
	CapabilitySupported   = settings.CapabilitySupported
	CapabilityUnsupported = settings.CapabilityUnsupported
)
