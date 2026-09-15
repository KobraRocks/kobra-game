package pack

import "testing"

// TestCompareSemver pins the ordering contract and, more importantly, the
// fail-closed rule for unreadable input.
//
// VERSIONING.md: an unparseable version fails its gate closed and is never
// silently ordered. This function used to discard strconv.Atoi's error, so
// "1.0.0-rc.1" parsed as 1.0.0 and compared EQUAL to its release — while the
// launcher's comparator orders that same input as older than everything. Two
// comparators disagreeing about a version is how a release passes the packager
// and is then refused, or wrongly accepted, at run time.
//
// The vectors below are shared with TestMeetsLauncherMin and
// TestMalformedLauncherMinCannotDisableTheFloor in the launcher module. The two
// modules cannot share code — the launcher keeps a three-dependency footprint and
// the packager is a separate tool that never ships — so the tables are kept in
// step by hand. Change one, change the other.
func TestCompareSemver(t *testing.T) {
	tests := []struct {
		a, b string
		want int
		ok   bool
	}{
		// Valid bare X.Y.Z: ordered numerically.
		{"1.4.0", "1.4.0", 0, true},
		{"1.3.9", "1.4.0", -1, true},
		{"1.4.1", "1.4.0", 1, true},
		{"2.0.0", "1.99.99", 1, true},
		{"0.1.0", "0.1.0", 0, true},

		// Unreadable input is refused, never coerced to zero. A suffixed version
		// is invalid by contract — pre-release status is the release manifest's
		// `channel` — rather than merely lower than its release.
		{"0.1.0-dev", "0.1.0", 0, false},
		{"1.0.0-rc.1", "1.0.0", 0, false},
		{"1.0.0+build", "1.0.0", 0, false},
		{"1.4", "1.4.0", 0, false},
		{"1.4.0.0", "1.4.0", 0, false},
		{"v1.4.0", "1.4.0", 0, false},
		{"", "1.4.0", 0, false},
		{"1.4.0", "garbage", 0, false},
	}
	for _, tc := range tests {
		got, ok := CompareSemver(tc.a, tc.b)
		if ok != tc.ok {
			t.Errorf("CompareSemver(%q, %q) ok = %v, want %v", tc.a, tc.b, ok, tc.ok)
			continue
		}
		if !ok {
			if got != 0 {
				t.Errorf("CompareSemver(%q, %q) = %d on unreadable input, want 0", tc.a, tc.b, got)
			}
			continue
		}
		if got != tc.want {
			t.Errorf("CompareSemver(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
