package settings

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCatalogueCandidatesConservative(t *testing.T) {
	c := Catalogue{ModelAliases: map[string]Model{
		"ok":  {Model: "p/m", Capabilities: Capabilities{Streaming: CapabilitySupported}, Tasks: []string{"coding"}},
		"no":  {Model: "p/n", Capabilities: Capabilities{Streaming: CapabilityUnsupported}},
		"unk": {Model: "p/u"},
		"off": {Model: "p/o", Enabled: boolPtr(false), Capabilities: Capabilities{Streaming: CapabilitySupported}},
	}}
	got := c.Candidates(Requirements{Streaming: true, Tasks: []string{"coding"}})
	assert.Len(t, got, 1)
	assert.Equal(t, "ok", got[0])
}

func TestSatisfiedByIsConservative(t *testing.T) {
	unknown := Model{Model: "p/m"}
	for _, req := range []Requirements{
		{Streaming: true}, {ToolCalling: true}, {StructuredOutput: true}, {Tasks: []string{"coding"}},
	} {
		assert.False(t, req.SatisfiedBy(unknown), "%+v was satisfied by a model that never claimed it", req)
	}
	// Enabled is opt-out, so a model without the field is usable.
	assert.True(t, unknown.IsEnabled(), "a model without the enabled field must be enabled")
}

func boolPtr(v bool) *bool { return &v }
