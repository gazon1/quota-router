package routing

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRouteRequestValidateAcceptsMinimalRequest(t *testing.T) {
	r := RouteRequest{Version: RouteRequestVersionV1, SessionID: "session-1", Capabilities: &RequiredCapabilities{}}
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestRouteRequestValidateAcceptsOptionalFields(t *testing.T) {
	r := RouteRequest{
		Version:   RouteRequestVersionV1,
		SessionID: "session-1",
		Capabilities: &RequiredCapabilities{
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
		{"wrong version", RouteRequest{Version: 0, SessionID: "s", Capabilities: &RequiredCapabilities{}}, "version must be 1"},
		{"future version", RouteRequest{Version: 2, SessionID: "s", Capabilities: &RequiredCapabilities{}}, "version must be 1"},
		{"missing session", RouteRequest{Version: RouteRequestVersionV1, Capabilities: &RequiredCapabilities{}}, "session_id must not be empty"},
		{"blank session", RouteRequest{Version: RouteRequestVersionV1, SessionID: "   ", Capabilities: &RequiredCapabilities{}}, "session_id must not be empty"},
		{"missing capabilities", RouteRequest{Version: RouteRequestVersionV1, SessionID: "s"}, "capabilities must be present"},
		{"unknown task class", RouteRequest{Version: RouteRequestVersionV1, SessionID: "s", Capabilities: &RequiredCapabilities{}, TaskClass: TaskClass("creative")}, "task_class \"creative\" is not supported"},
		{"empty preference", RouteRequest{Version: RouteRequestVersionV1, SessionID: "s", Capabilities: &RequiredCapabilities{}, Preference: &RoutePreference{}}, "preference must set provider or model_alias"},
		{"blank preference provider", RouteRequest{Version: RouteRequestVersionV1, SessionID: "s", Capabilities: &RequiredCapabilities{}, Preference: &RoutePreference{Provider: "  "}}, "preference.provider must not be blank"},
		{"blank preference alias", RouteRequest{Version: RouteRequestVersionV1, SessionID: "s", Capabilities: &RequiredCapabilities{}, Preference: &RoutePreference{ModelAlias: "\t"}}, "preference.model_alias must not be blank"},
		{"empty override", RouteRequest{Version: RouteRequestVersionV1, SessionID: "s", Capabilities: &RequiredCapabilities{}, Override: &ManualOverride{}}, "override.model_alias must not be empty"},
		{"blank override", RouteRequest{Version: RouteRequestVersionV1, SessionID: "s", Capabilities: &RequiredCapabilities{}, Override: &ManualOverride{ModelAlias: " "}}, "override.model_alias must not be empty"},
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
		r := RouteRequest{Version: version, SessionID: "session-1", Capabilities: &RequiredCapabilities{}}
		err := r.Validate()
		if err == nil || !strings.Contains(err.Error(), "version must be 1") {
			t.Fatalf("Validate(version=%d) = %v, want version error", version, err)
		}
	}
}

func TestRouteRequestValidateRejectsBlankSessionIDs(t *testing.T) {
	for _, sessionID := range []string{"", " ", "   ", "\t", "\n", " \t\n "} {
		r := RouteRequest{Version: RouteRequestVersionV1, SessionID: sessionID, Capabilities: &RequiredCapabilities{}}
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
				Version:      RouteRequestVersionV1,
				SessionID:    "session-1",
				Capabilities: &RequiredCapabilities{},
				Preference:   tc.preference,
				Override:     tc.override,
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
		Version:      RouteRequestVersionV1,
		SessionID:    "s",
		Capabilities: &RequiredCapabilities{},
		Preference:   &RoutePreference{Provider: "minimax"},
		Override:     &ManualOverride{ModelAlias: "free-coding"},
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestRouteRequestJSONContract(t *testing.T) {
	t.Run("missing capabilities is rejected", func(t *testing.T) {
		var r RouteRequest
		if err := json.Unmarshal([]byte(`{"version":1,"session_id":"s"}`), &r); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if r.Capabilities != nil {
			t.Fatalf("Capabilities = %+v, want nil for missing field", r.Capabilities)
		}
		err := r.Validate()
		if err == nil || !strings.Contains(err.Error(), "capabilities must be present") {
			t.Fatalf("Validate() = %v, want missing capabilities error", err)
		}
	})

	t.Run("empty capabilities object is valid", func(t *testing.T) {
		var r RouteRequest
		if err := json.Unmarshal([]byte(`{"version":1,"session_id":"s","capabilities":{}}`), &r); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if r.Capabilities == nil {
			t.Fatal("Capabilities = nil, want present empty object")
		}
		if err := r.Validate(); err != nil {
			t.Fatalf("Validate() = %v, want nil", err)
		}
	})

	t.Run("field names and omitempty", func(t *testing.T) {
		r := RouteRequest{
			Version:      RouteRequestVersionV1,
			SessionID:    "session-1",
			Capabilities: &RequiredCapabilities{ToolCalling: true},
			TaskClass:    TaskCoding,
			Preference:   &RoutePreference{ModelAlias: "free-coding"},
			Override:     &ManualOverride{ModelAlias: "free-coding"},
		}
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("Unmarshal map: %v", err)
		}
		for _, key := range []string{"version", "session_id", "capabilities", "task_class", "preference", "override"} {
			if _, ok := got[key]; !ok {
				t.Fatalf("missing key %q in %s", key, b)
			}
		}
		caps := got["capabilities"].(map[string]any)
		if _, ok := caps["tool_calling"]; !ok {
			t.Fatalf("missing capabilities.tool_calling in %s", b)
		}
		if _, ok := caps["streaming"]; ok {
			t.Fatalf("streaming should be omitted when false: %s", b)
		}
		if _, ok := caps["structured_output"]; ok {
			t.Fatalf("structured_output should be omitted when false: %s", b)
		}

		minimal, err := json.Marshal(RouteRequest{Version: RouteRequestVersionV1, SessionID: "s", Capabilities: &RequiredCapabilities{}})
		if err != nil {
			t.Fatalf("Marshal minimal: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(minimal, &m); err != nil {
			t.Fatalf("Unmarshal minimal: %v", err)
		}
		for _, absent := range []string{"task_class", "preference", "override"} {
			if _, ok := m[absent]; ok {
				t.Fatalf("%q should be omitted: %s", absent, minimal)
			}
		}
	})
}
