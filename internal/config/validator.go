package config

import (
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/go-playground/validator/v10"
)

var (
	configValidatorOnce sync.Once
	configValidator     *validator.Validate
)

func getValidator() *validator.Validate {
	configValidatorOnce.Do(func() {
		configValidator = newValidator()
	})
	return configValidator
}

func newValidator() *validator.Validate {
	v := validator.New(validator.WithRequiredStructEnabled())
	v.RegisterTagNameFunc(func(fld reflect.StructField) string {
		name := strings.SplitN(fld.Tag.Get("koanf"), ",", 2)[0]
		if name == "" || name == "-" {
			return fld.Name
		}
		return name
	})
	mustRegisterValidation(v, "http_url", validateHTTPURL)
	mustRegisterValidation(v, "sqlite_path", validateSQLitePath)
	mustRegisterValidation(v, "model_id", validateModelID)
	mustRegisterValidation(v, "alias_name", validateAliasName)
	mustRegisterValidation(v, "iana_timezone", validateIANATimezone)
	mustRegisterValidation(v, "weekday", validateWeekday)
	mustRegisterValidation(v, "clock_hhmm", validateClockHHMM)

	v.RegisterStructValidation(validateConfigStruct, Config{})
	v.RegisterStructValidation(validateRoutingStruct, Routing{})
	v.RegisterStructValidation(validateProviderRoutingStruct, ProviderRouting{})
	v.RegisterStructValidation(validateModelStruct, Model{})
	v.RegisterStructValidation(validateUnavailableStruct, Unavailable{})
	v.RegisterStructValidation(validateUnavailableWindowStruct, UnavailableWindow{})
	return v
}

func mustRegisterValidation(v *validator.Validate, tag string, fn validator.Func) {
	if err := v.RegisterValidation(tag, fn); err != nil {
		panic(fmt.Sprintf("register validation %s: %v", tag, err))
	}
}

func validateHTTPURL(fl validator.FieldLevel) bool {
	raw := fl.Field().String()
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	// The endpoint is built by appending a path to this value, so a query or
	// fragment would swallow it: https://proxy?x=1/v1/models is not the route.
	// ForceQuery catches a bare trailing "?"; url.Parse drops a bare trailing
	// "#", so the raw value is the only witness for that one.
	return u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && !strings.HasSuffix(raw, "#")
}

func validateSQLitePath(fl validator.FieldLevel) bool {
	p := fl.Field().String()
	if p == "" || strings.HasPrefix(p, ":") || strings.HasPrefix(strings.ToLower(p), "file:") {
		return false
	}
	return true
}

func validateModelID(fl validator.FieldLevel) bool {
	m := fl.Field().String()
	provider, name, found := strings.Cut(m, "/")
	return found && provider != "" && name != ""
}

func validateAliasName(fl validator.FieldLevel) bool {
	name := fl.Field().String()
	return name != "" && !strings.ContainsAny(name, "/ \t")
}

func validateIANATimezone(fl validator.FieldLevel) bool {
	name := fl.Field().String()
	if name == "" {
		return true
	}
	if name == "Local" {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}

func validateWeekday(fl validator.FieldLevel) bool {
	name := strings.ToLower(strings.TrimSpace(fl.Field().String()))
	switch name {
	case "sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday":
		return true
	default:
		return false
	}
}

func validateClockHHMM(fl validator.FieldLevel) bool {
	_, err := time.Parse("15:04", fl.Field().String())
	return err == nil
}
