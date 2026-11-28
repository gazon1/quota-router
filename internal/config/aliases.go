package config

import "github.com/gazon1/quota-router/internal/settings"

// Tag-free runtime types are re-exported so config's own API keeps naming them
// without a settings import at every call site. Types that carry the
// configuration shape (Model, Capabilities, Routing, ProviderRouting, Provider,
// Unavailable, UnavailableWindow) are declared here instead, in model.go and
// routing.go.
type (
	Secret       = settings.Secret
	Capability   = settings.Capability
	Requirements = settings.Requirements
)

const (
	CapabilityUnknown     = settings.CapabilityUnknown
	CapabilitySupported   = settings.CapabilitySupported
	CapabilityUnsupported = settings.CapabilityUnsupported
)
