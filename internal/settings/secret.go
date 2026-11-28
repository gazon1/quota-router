// Package settings holds dependency-light runtime data types shared outside
// configuration loading: model catalogue, routing policy, and the Secret
// wrapper.
//
// It has no Koanf or environment-loading dependency, and no configuration
// format metadata: no koanf or validate struct tags. internal/config remains
// the single owner of loading, defaults, validation, normalization, and
// secret-file resolution, and its Catalogue method converts its own
// configuration types into these.
package settings

// Secret is a credential: an API key or token read from the config file,
// environment, or a mounted secret file. It prints and marshals as a redacted
// value so it cannot leak through a log line or an error message.
type Secret string

const redacted = "[REDACTED]"

// String keeps Secret out of formatted output.
func (Secret) String() string { return redacted }

// GoString keeps Secret out of %#v output.
func (Secret) GoString() string { return redacted }

// LogValue keeps Secret out of slog attributes.
func (Secret) LogValue() any { return redacted }

// MarshalText keeps Secret out of JSON, including slog's JSON handler, which
// marshals a value that implements encoding.TextMarshaler as a JSON string.
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// UnmarshalText lets the loader populate a Secret from a plain string value.
func (s *Secret) UnmarshalText(b []byte) error {
	*s = Secret(b)
	return nil
}
