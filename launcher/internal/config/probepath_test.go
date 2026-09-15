package config

import "testing"

// TestValidateProbePathRejectsAnUnservedEndpoint pins the fix for a silently
// inert key: server.probe_path is published in the schema as configurable, but
// the endpoint is fixed by the data API the shell speaks, so a different value
// must be refused rather than accepted and ignored.
func TestValidateProbePathRejectsAnUnservedEndpoint(t *testing.T) {
	const served = "/__kobra/probe"

	cases := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"absent key is fine", "", false},
		{"the served path is fine", served, false},
		{"a moved path is refused", "/__custom/probe", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{Server: ServerConfig{ProbePath: tc.value}}
			err := c.ValidateProbePath(served)
			if tc.wantErr && err == nil {
				t.Fatalf("ValidateProbePath(%q) accepted an endpoint the launcher does not serve", tc.value)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateProbePath(%q) = %v, want nil", tc.value, err)
			}
		})
	}
}
