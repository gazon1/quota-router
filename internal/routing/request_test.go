package routing

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRouteRequestValidateAcceptsMinimalRequest(t *testing.T) {
	r := RouteRequest{Version: RouteRequestVersionV1, SessionID: "session-1", Capabilities: &RequiredCapabilities{}}
	require.NoError(t, r.Validate())
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
	require.NoError(t, r.Validate())
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
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

func TestRouteRequestValidateRejectsInvalidVersions(t *testing.T) {
	for _, version := range []int{-1, 0, 2, 99} {
		r := RouteRequest{Version: version, SessionID: "session-1", Capabilities: &RequiredCapabilities{}}
		err := r.Validate()
		require.Error(t, err, "Validate(version=%d)", version)
		assert.ErrorContains(t, err, "version must be 1")
	}
}

func TestRouteRequestValidateRejectsBlankSessionIDs(t *testing.T) {
	for _, sessionID := range []string{"", " ", "   ", "\t", "\n", " \t\n "} {
		r := RouteRequest{Version: RouteRequestVersionV1, SessionID: sessionID, Capabilities: &RequiredCapabilities{}}
		err := r.Validate()
		require.Error(t, err, "Validate(session_id=%q)", sessionID)
		assert.ErrorContains(t, err, "session_id must not be empty")
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
			require.Error(t, err)
			assert.ErrorContains(t, err, "contradicts override.model_alias")
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
	require.NoError(t, r.Validate())
}

func TestRouteRequestJSONContract(t *testing.T) {
	t.Run("missing capabilities is rejected", func(t *testing.T) {
		var r RouteRequest
		require.NoError(t, json.Unmarshal([]byte(`{"version":1,"session_id":"s"}`), &r))
		assert.Nil(t, r.Capabilities, "want nil for missing field")
		err := r.Validate()
		require.Error(t, err)
		assert.ErrorContains(t, err, "capabilities must be present")
	})

	t.Run("empty capabilities object is valid", func(t *testing.T) {
		var r RouteRequest
		require.NoError(t, json.Unmarshal([]byte(`{"version":1,"session_id":"s","capabilities":{}}`), &r))
		assert.NotNil(t, r.Capabilities, "want present empty object")
		require.NoError(t, r.Validate())
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
		require.NoError(t, err)
		var got map[string]any
		require.NoError(t, json.Unmarshal(b, &got))
		for _, key := range []string{"version", "session_id", "capabilities", "task_class", "preference", "override"} {
			assert.Contains(t, got, key, "missing key in %s", b)
		}
		caps := got["capabilities"].(map[string]any)
		assert.Contains(t, caps, "tool_calling", "missing capabilities.tool_calling in %s", b)
		assert.NotContains(t, caps, "streaming", "streaming should be omitted when false: %s", b)
		assert.NotContains(t, caps, "structured_output", "structured_output should be omitted when false: %s", b)

		minimal, err := json.Marshal(RouteRequest{Version: RouteRequestVersionV1, SessionID: "s", Capabilities: &RequiredCapabilities{}})
		require.NoError(t, err)
		var m map[string]any
		require.NoError(t, json.Unmarshal(minimal, &m))
		for _, absent := range []string{"task_class", "preference", "override"} {
			assert.NotContains(t, m, absent, "should be omitted: %s", minimal)
		}
	})
}
