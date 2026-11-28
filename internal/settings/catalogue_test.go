package settings

import "testing"

func TestCatalogueCandidatesConservative(t *testing.T) {
	c := Catalogue{ModelAliases: map[string]Model{
		"ok":  {Model: "p/m", Capabilities: Capabilities{Streaming: CapabilitySupported}, Tasks: []string{"coding"}},
		"no":  {Model: "p/n", Capabilities: Capabilities{Streaming: CapabilityUnsupported}},
		"unk": {Model: "p/u"},
		"off": {Model: "p/o", Enabled: boolPtr(false), Capabilities: Capabilities{Streaming: CapabilitySupported}},
	}}
	got := c.Candidates(Requirements{Streaming: true, Tasks: []string{"coding"}})
	if len(got) != 1 || got[0] != "ok" {
		t.Fatalf("Candidates = %v, want [ok]", got)
	}
}

func boolPtr(v bool) *bool { return &v }
