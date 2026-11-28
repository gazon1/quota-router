package config

// Unavailable validation is enforced at the config validation boundary via
// struct tags and config-owned custom/struct validators. Runtime behavior
// (WindowAt/String) remains in internal/settings.
