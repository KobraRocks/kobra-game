package main

import "testing"

// TestParsePortRequiresTheWholeString is the regression test for the flag
// parser: fmt.Sscanf("%d") stops at the first non-digit, so "--port 8080abc"
// used to be accepted as 8080.
func TestParsePortRequiresTheWholeString(t *testing.T) {
	bad := []string{"", "abc", "8080abc", "80 80", "-1", "0", "65536", "99999999999999999999"}
	for _, s := range bad {
		if got, err := parsePort(s); err == nil {
			t.Errorf("parsePort(%q) = %d, nil; want an error", s, got)
		}
	}
	good := map[string]uint16{"1": 1, "8080": 8080, "65535": 65535, " 8080 ": 8080, "018080": 18080}
	for in, want := range good {
		got, err := parsePort(in)
		if err != nil {
			t.Errorf("parsePort(%q) = %v, want %d", in, err, want)
			continue
		}
		if got != want {
			t.Errorf("parsePort(%q) = %d, want %d", in, got, want)
		}
	}
}
