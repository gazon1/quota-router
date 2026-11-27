package routing

import (
	"strings"
	"testing"
)

func TestRouteRequestValidateAcceptsMinimalRequest(t *testing.T) {
	r := RouteRequest{Version: RouteRequestVersionV1, SessionID: "session-1"}
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestRouteRequestValidateAcceptsOptionalFields(t *testing.T) {
	r := RouteRequest{
		Version:   RouteRequestVersionV1,
		SessionID: "session-1",
		Capabilities: RequiredCapabilities{
			Streaming:        true,
			ToolCalling:      true,
			StructuredOutput: false,
		},
		TaskClass:  TaskCoding,
		Preference: &RoutePreference{Provider: "commandcode", ModelAlias: "free-coding"},
		Override:   &ManualOverride{ModelAlias: "free-coding"},
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestRouteRequestValidateRejectsMalformedAndUnsupported(t *testing.T) {
	tests := []struct {
		name string
		req  RouteRequest
		want string
	}{
		{"wrong version", RouteRequest{Version: 0, SessionID: "s"}, "version must be 1"},
		{"future version", RouteRequest{Version: 2, SessionID: "s"}, "version must be 1"},
		{"missing session", RouteRequest{Version: RouteRequestVersionV1}, "session_id must not be empty"},
		{"blank session", RouteRequest{Version: RouteRequestVersionV1, SessionID: "   "}, "session_id must not be empty"},
		{"unknown task class", RouteRequest{Version: RouteRequestVersionV1, SessionID: "s", TaskClass: TaskClass("creative")}, "task_class \"creative\" is not supported"},
		{"empty preference", RouteRequest{Version: RouteRequestVersionV1, SessionID: "s", Preference: &RoutePreference{}}, "preference must set provider or model_alias"},
		{"blank preference provider", RouteRequest{Version: RouteRequestVersionV1, SessionID: "s", Preference: &RoutePreference{Provider: "  "}}, "preference.provider must not be blank"},
		{"blank preference alias", RouteRequest{Version: RouteRequestVersionV1, SessionID: "s", Preference: &RoutePreference{ModelAlias: "\t"}}, "preference.model_alias must not be blank"},
		{"empty override", RouteRequest{Version: RouteRequestVersionV1, SessionID: "s", Override: &ManualOverride{}}, "override.model_alias must not be empty"},
		{"blank override", RouteRequest{Version: RouteRequestVersionV1, SessionID: "s", Override: &ManualOverride{ModelAlias: " "}}, "override.model_alias must not be empty"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestRouteRequestValidateRejectsInvalidVersions(t *testing.T) {
	for _, version := range []int{-1, 0, 2, 99} {
		r := RouteRequest{Version: version, SessionID: "session-1"}
		err := r.Validate()
		if err == nil || !strings.Contains(err.Error(), "version must be 1") {
			t.Fatalf("Validate(version=%d) = %v, want version error", version, err)
		}
	}
}

func TestRouteRequestValidateRejectsBlankSessionIDs(t *testing.T) {
	for _, sessionID := range []string{"", " ", "   ", "\t", "\n", " \t\n "} {
		r := RouteRequest{Version: RouteRequestVersionV1, SessionID: sessionID}
		err := r.Validate()
		if err == nil || !strings.Contains(err.Error(), "session_id must not be empty") {
			t.Fatalf("Validate(session_id=%q) = %v, want session_id error", sessionID, err)
		}
	}
}

func TestRouteRequestValidateRejectsContradictoryPreferences(t *testing.T) {
	tests := []struct {
		name       string
		preference *RoutePreference
		override   *ManualOverride
	}{
		{
			name:       "different model aliases",
			preference: &RoutePreference{ModelAlias: "fast"},
			override:   &ManualOverride{ModelAlias: "free-coding"},
		},
		{
			name:       "provider preference does not hide model alias contradiction",
			preference: &RoutePreference{Provider: "commandcode", ModelAlias: "fast"},
			override:   &ManualOverride{ModelAlias: "free-coding"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := RouteRequest{
				Version:    RouteRequestVersionV1,
				SessionID:  "session-1",
				Preference: tc.preference,
				Override:   tc.override,
			}
			err := r.Validate()
			if err == nil || !strings.Contains(err.Error(), "contradicts override.model_alias") {
				t.Fatalf("Validate() = %v, want contradiction", err)
			}
		})
	}
}

func TestRouteRequestValidateAllowsProviderPreferenceWithDifferentOverride(t *testing.T) {
	// Provider preference is not a hard contradiction with a specific override
	// alias; selection (#17) decides whether the override target satisfies it.
	r := RouteRequest{
		Version:    RouteRequestVersionV1,
		SessionID:  "s",
		Preference: &RoutePreference{Provider: "minimax"},
		Override:   &ManualOverride{ModelAlias: "free-coding"},
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}
