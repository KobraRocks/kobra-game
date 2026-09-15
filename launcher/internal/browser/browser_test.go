package browser

// Tests for §20 detection, version gating and launch.
//
// Detection tests are hermetic: they point the package's search plan at a
// temporary directory and a temporary $PATH, so they pass whether or not a
// browser is installed on the machine running them. Because `plan` is a package
// variable, these tests must not run in parallel (no t.Parallel anywhere here).

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kobragames.local/launcher/internal/config"
	"kobragames.local/launcher/internal/diagnostics"
)

func minVersions() config.MinBrowserVersion {
	return config.MinBrowserVersion{Chrome: 105, Edge: 105, Opera: 91, Brave: "1.45"}
}

func setPlan(t *testing.T, p searchPlan) {
	t.Helper()
	saved := plan
	t.Cleanup(func() { plan = saved })
	plan = p
}

// fakeBin creates a temporary directory used both as the only searched directory
// and as $PATH, and returns it with the production bare-name lists (so the fake
// files carry the real executable names: google-chrome, brave-browser, ...).
func fakeBin(t *testing.T) (string, map[string][]string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create fake bin: %v", err)
	}
	t.Setenv("PATH", dir)
	return dir, plan.names
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// writeScript writes an executable /bin/sh script that prints stdout.
func writeScript(t *testing.T, dir, name, stdout string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	body := "#!/bin/sh\nprintf '%s\\n' " + shellQuote(stdout) + "\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake browser %s: %v", name, err)
	}
	return p
}

func candidateNames(cs []Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}

func TestWatchdogTimeout(t *testing.T) {
	// §20.5: 20 seconds from launch to the first heartbeat, fired once.
	if WatchdogTimeout != 20*time.Second {
		t.Fatalf("WatchdogTimeout = %v, want 20s (§20.5)", WatchdogTimeout)
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		version string
		major   int
		ok      bool
	}{
		{"chrome linux", "Google Chrome 117.0.5938.132\n", "117.0.5938.132", 117, true},
		{"chromium linux", "Chromium 117.0.5938.132\n", "117.0.5938.132", 117, true},
		{"chromium dev build", "Chromium 118.0.5993.70 (Developer Build) Fedora Project\n", "118.0.5993.70", 118, true},
		{"edge linux", "Microsoft Edge 117.0.2045.47\n", "117.0.2045.47", 117, true},
		// Brave prints its own version first and the bundled Chromium second;
		// the Brave floor must be compared against 1.45.116, not 117.0.5938.132.
		{"brave linux", "Brave Browser 1.45.116 Chromium: 117.0.5938.132\n", "1.45.116", 1, true},
		{"opera linux", "Opera 91.0.4516.20\n", "91.0.4516.20", 91, true},
		{"three component version", "Google Chrome 105.0.0\n", "105.0.0", 105, true},
		{"version on stderr-style prefix", "ERROR: unknown flag\nGoogle Chrome 120.0.6099.109\n", "120.0.6099.109", 120, true},
		{"empty", "", "", 0, false},
		{"no version", "Usage: chrome [options]\n", "", 0, false},
		{"two components only", "Google Chrome 117.0\n", "", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			version, major, ok := parseVersion(tc.out)
			if ok != tc.ok || version != tc.version || major != tc.major {
				t.Fatalf("parseVersion(%q) = (%q, %d, %v), want (%q, %d, %v)",
					tc.out, version, major, ok, tc.version, tc.major, tc.ok)
			}
		})
	}
}

func TestMinimumMajor(t *testing.T) {
	min := minVersions()
	tests := []struct {
		name     string
		floor    int
		hasFloor bool
	}{
		{"chrome", 105, true},
		{"chromium", 105, true}, // config has no chromium field: Chrome's floor applies
		{"msedge", 105, true},
		{"opera", 91, true},
		{"brave", 1, true}, // "1.45" -> major 1, per the documented interpretation
	}
	for _, tc := range tests {
		floor, has := minimumMajor(tc.name, min)
		if floor != tc.floor || has != tc.hasFloor {
			t.Errorf("minimumMajor(%q) = (%d, %v), want (%d, %v)", tc.name, floor, has, tc.floor, tc.hasFloor)
		}
	}

	// Brave's string minimum is reduced to its major component.
	for _, tc := range []struct {
		brave string
		want  int
		ok    bool
	}{
		{"1.45", 1, true},
		{"2.7.1", 2, true},
		{"3", 3, true},
		{"", 0, false},
		{"not-a-version", 0, false},
		{" 1.45 ", 1, true},
	} {
		got, ok := parseMajorString(tc.brave)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseMajorString(%q) = (%d, %v), want (%d, %v)", tc.brave, got, ok, tc.want, tc.ok)
		}
	}

	// An unset minimum means "no floor", not "reject everything".
	if _, has := minimumMajor("chrome", config.MinBrowserVersion{}); has {
		t.Error("minimumMajor(chrome) with a zero minimum reported a floor")
	}
	if _, has := minimumMajor("brave", config.MinBrowserVersion{Brave: ""}); has {
		t.Error("minimumMajor(brave) with a blank minimum reported a floor")
	}
}

func TestProbeOrder(t *testing.T) {
	tests := []struct {
		name string
		pref []string
		want []string
	}{
		{"empty falls back to defaults", nil, defaultPreference},
		{"aliases and case", []string{"Edge", "GOOGLE-CHROME", "edge", "Chrome"}, []string{"msedge", "chrome"}},
		{"deduplicated in pref order", []string{"brave", "chrome", "brave", "google-chrome"}, []string{"brave", "chrome"}},
		{"unknown targets are dropped", []string{"vivaldi", "firefox", "custom"}, nil},
		{"known target alongside unknown", []string{"vivaldi", "chromium"}, []string{"chromium"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := probeOrder(tc.pref)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("probeOrder(%v) = %v, want %v", tc.pref, got, tc.want)
			}
		})
	}

	// Every canonical target has an alias entry, and defaults are canonical.
	canonical := map[string]bool{}
	for _, n := range canonicalNames {
		canonical[n] = true
	}
	for _, n := range defaultPreference {
		if !canonical[n] {
			t.Errorf("defaultPreference contains non-canonical name %q", n)
		}
	}
	for alias, target := range aliasToCanonical {
		if !canonical[target] {
			t.Errorf("alias %q maps to non-canonical name %q", alias, target)
		}
	}
}

func TestDetectPrefersPreferenceOrder(t *testing.T) {
	dir, names := fakeBin(t)
	setPlan(t, searchPlan{dirs: []string{dir}, names: names})

	writeScript(t, dir, names["chrome"][0], "Google Chrome 117.0.5938.132")
	writeScript(t, dir, names["chromium"][0], "Chromium 120.0.6099.109")
	writeScript(t, dir, names["brave"][0], "Brave Browser 1.62.156 Chromium: 117.0.5938.132")

	got, err := Detect([]string{"brave", "chromium", "chrome"}, minVersions())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	wantNames := []string{"brave", "chromium", "chrome"}
	if names := candidateNames(got); strings.Join(names, ",") != strings.Join(wantNames, ",") {
		t.Fatalf("Detect order = %v, want %v", names, wantNames)
	}
	wantVersions := []string{"1.62.156", "120.0.6099.109", "117.0.5938.132"}
	wantMajors := []int{1, 120, 117}
	for i, c := range got {
		if c.Version != wantVersions[i] || c.Major != wantMajors[i] {
			t.Errorf("candidate %d = %+v, want version %q major %d", i, c, wantVersions[i], wantMajors[i])
		}
		if c.Path == "" {
			t.Errorf("candidate %d has an empty path", i)
		}
	}

	// Duplicates in pref produce one candidate each, in first-seen order.
	deduped, err := Detect([]string{"chrome", "chrome", "google-chrome", "brave"}, minVersions())
	if err != nil {
		t.Fatalf("Detect (dedup): %v", err)
	}
	if names := candidateNames(deduped); strings.Join(names, ",") != "chrome,brave" {
		t.Fatalf("Detect (dedup) = %v, want [chrome brave]", names)
	}
}

func TestDetectSkipsTooOldCandidates(t *testing.T) {
	dir, names := fakeBin(t)
	setPlan(t, searchPlan{dirs: []string{dir}, names: names})

	writeScript(t, dir, names["chrome"][0], "Google Chrome 100.0.4896.127")
	writeScript(t, dir, names["msedge"][0], "Microsoft Edge 90.0.818.66")
	writeScript(t, dir, names["brave"][0], "Brave Browser 1.62.156 Chromium: 117.0.5938.132")

	got, err := Detect([]string{"chrome", "msedge", "brave"}, minVersions())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if names := candidateNames(got); strings.Join(names, ",") != "brave" {
		t.Fatalf("Detect = %v, want only [brave] (chrome and edge are below the minimum)", names)
	}
}

func TestBraveFloorComparesMajorOnly(t *testing.T) {
	dir, names := fakeBin(t)
	setPlan(t, searchPlan{dirs: []string{dir}, names: names})

	// Documented interpretation: the installed Brave major is compared with the
	// major of config Brave ("1.45" -> 1), so a 1.44 install passes a 1.45 floor.
	writeScript(t, dir, names["brave"][0], "Brave Browser 1.44.112 Chromium: 116.0.5845.96")

	got, err := Detect([]string{"brave"}, minVersions())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(got) != 1 || got[0].Major != 1 {
		t.Fatalf("Detect = %+v, want one brave candidate with major 1", got)
	}
}

func TestDetectReturnsErrTooOldWhenAllTooOld(t *testing.T) {
	dir, names := fakeBin(t)
	setPlan(t, searchPlan{dirs: []string{dir}, names: names})

	writeScript(t, dir, names["chrome"][0], "Google Chrome 100.0.4896.127")
	writeScript(t, dir, names["chromium"][0], "Chromium 99.0.4844.51")

	got, err := Detect([]string{"chrome", "chromium"}, minVersions())
	if !errors.Is(err, ErrTooOld) {
		t.Fatalf("Detect error = %v, want ErrTooOld", err)
	}
	if errors.Is(err, ErrNoBrowser) {
		t.Fatalf("Detect error = %v, must not also be ErrNoBrowser", err)
	}
	if len(got) != 0 {
		t.Fatalf("Detect returned %v with an error", got)
	}
}

func TestDetectReturnsErrNoBrowser(t *testing.T) {
	_, names := fakeBin(t)
	setPlan(t, searchPlan{dirs: []string{t.TempDir()}, names: names}) // deliberately empty

	got, err := Detect([]string{"chrome", "brave"}, minVersions())
	if !errors.Is(err, ErrNoBrowser) {
		t.Fatalf("Detect error = %v, want ErrNoBrowser", err)
	}
	if len(got) != 0 {
		t.Fatalf("Detect returned %v with an error", got)
	}
}

func TestDetectTreatsUnknownVersionAsUnsupported(t *testing.T) {
	dir, names := fakeBin(t)
	setPlan(t, searchPlan{dirs: []string{dir}, names: names})

	writeScript(t, dir, names["chrome"][0], "chrome: unrecognised option '--version'")
	writeScript(t, dir, names["brave"][0], "Brave Browser 1.62.156 Chromium: 117.0.5938.132")

	got, err := Detect([]string{"chrome", "brave"}, minVersions())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if names := candidateNames(got); strings.Join(names, ",") != "brave" {
		t.Fatalf("Detect = %v, want only [brave]: a version-less browser is unsupported (§20.1)", names)
	}

	// Alone, the version-less browser must not be returned at all.
	got, err = Detect([]string{"chrome"}, minVersions())
	if len(got) != 0 || !errors.Is(err, ErrTooOld) {
		t.Fatalf("Detect = (%v, %v), want no candidates and ErrTooOld", got, err)
	}

	// Unsupported preference targets are not silently replaced by defaults.
	if _, err := Detect([]string{"vivaldi"}, minVersions()); !errors.Is(err, ErrNoBrowser) {
		t.Fatalf("Detect(vivaldi) error = %v, want ErrNoBrowser", err)
	}
}

func TestDetectPrefersAbsolutePathsOverPath(t *testing.T) {
	names := plan.names
	priorityDir := filepath.Join(t.TempDir(), "priority")
	pathDir := filepath.Join(t.TempDir(), "path")
	for _, d := range []string{priorityDir, pathDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	writeScript(t, priorityDir, names["chrome"][0], "Google Chrome 118.0.5993.70")
	writeScript(t, pathDir, names["chrome"][0], "Google Chrome 100.0.4896.127")
	t.Setenv("PATH", pathDir)
	setPlan(t, searchPlan{dirs: []string{priorityDir}, names: names})

	got, err := Detect([]string{"chrome"}, minVersions())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(got) != 1 || got[0].Version != "118.0.5993.70" {
		t.Fatalf("Detect = %+v, want the absolute-path browser (118.0.5993.70) ahead of $PATH", got)
	}
	if filepath.Dir(got[0].Path) != priorityDir {
		t.Fatalf("Detect path dir = %s, want the searched directory before $PATH", filepath.Dir(got[0].Path))
	}
}

func TestDetectFindsFlatpakExport(t *testing.T) {
	names := plan.names
	flatpakDir := filepath.Join(t.TempDir(), "flatpak-exports")
	emptyPath := filepath.Join(t.TempDir(), "empty")
	for _, d := range []string{flatpakDir, emptyPath} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	const flatpakID = "com.brave.Browser"
	writeScript(t, flatpakDir, flatpakID, "Brave Browser 1.62.156 Chromium: 117.0.5938.132")
	t.Setenv("PATH", emptyPath)
	setPlan(t, searchPlan{
		flatpakDirs: []string{flatpakDir},
		flatpakIDs:  map[string][]string{"brave": {flatpakID}},
		names:       names,
	})

	got, err := Detect([]string{"brave"}, minVersions())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Detect = %+v, want one flatpak candidate", got)
	}
	if got[0].Path != filepath.Join(flatpakDir, flatpakID) {
		t.Fatalf("Detect path = %s, want the flatpak export", got[0].Path)
	}
}

func TestDetectLogsNameAndVersionButNeverThePath(t *testing.T) {
	dir, names := fakeBin(t)
	setPlan(t, searchPlan{dirs: []string{dir}, names: names})
	writeScript(t, dir, names["chrome"][0], "Google Chrome 117.0.5938.132")

	lg, err := diagnostics.Open(diagnostics.Options{LogDir: t.TempDir(), Level: diagnostics.LevelDebug})
	if err != nil {
		t.Fatalf("diagnostics.Open: %v", err)
	}
	t.Cleanup(func() {
		SetLogger(nil)
		_ = lg.Close()
	})
	SetLogger(lg)

	if _, err := Detect([]string{"chrome"}, minVersions()); err != nil {
		t.Fatalf("Detect: %v", err)
	}

	lines := strings.Join(lg.Tail(0), "\n")
	for _, want := range []string{"browser.detected", `"name":"chrome"`, "117.0.5938.132"} {
		if !strings.Contains(lines, want) {
			t.Errorf("log does not contain %s:\n%s", want, lines)
		}
	}
	if strings.Contains(lines, dir) {
		t.Errorf("log leaked the browser path:\n%s", lines)
	}
}

func TestLaunchDoesNotWaitAndLogsName(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "launched.txt")
	exe := filepath.Join(dir, "brave-browser")
	script := "#!/bin/sh\nprintf '%s' \"$1\" > \"$KOBRA_BROWSER_TEST_OUT\"\nsleep 3\n"
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake browser: %v", err)
	}
	t.Setenv("KOBRA_BROWSER_TEST_OUT", out)

	lg, err := diagnostics.Open(diagnostics.Options{LogDir: t.TempDir(), Level: diagnostics.LevelDebug})
	if err != nil {
		t.Fatalf("diagnostics.Open: %v", err)
	}
	t.Cleanup(func() {
		SetLogger(nil)
		_ = lg.Close()
	})
	SetLogger(lg)

	const url = "http://127.0.0.1:8771/index.html#t=abc"
	start := time.Now()
	if err := Launch(exe, url); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Launch blocked for %v; it must use Start and not wait for the browser (§20.3)", elapsed)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		b, err := os.ReadFile(out)
		if err == nil && len(b) > 0 {
			if string(b) != url {
				t.Fatalf("browser received %q, want %q", b, url)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the launched browser never received the URL")
		}
		time.Sleep(10 * time.Millisecond)
	}

	lines := strings.Join(lg.Tail(0), "\n")
	for _, want := range []string{"browser.launch", `"name":"brave"`} {
		if !strings.Contains(lines, want) {
			t.Errorf("log does not contain %s:\n%s", want, lines)
		}
	}
	if strings.Contains(lines, dir) {
		t.Errorf("log leaked the browser path:\n%s", lines)
	}
}

func TestLaunchErrorIsPathFree(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "no-such-chrome")

	err := Launch(missing, "http://127.0.0.1:8771/index.html")
	if err == nil {
		t.Fatal("Launch of a missing executable returned nil")
	}
	if strings.Contains(err.Error(), dir) || strings.Contains(err.Error(), "no-such-chrome") {
		t.Fatalf("Launch error leaks a filesystem path: %v", err)
	}
	var le *LaunchError
	if !errors.As(err, &le) {
		t.Fatalf("Launch error type = %T, want *LaunchError", err)
	}
	if le.Cause() == nil {
		t.Fatal("LaunchError.Cause() is nil; operator diagnostics need the underlying error")
	}
}

func TestNameFromPath(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/usr/bin/google-chrome", "chrome"},
		{"/usr/bin/google-chrome-stable", "chrome"},
		{"/opt/google/chrome/chrome", "chrome"},
		{"/usr/bin/chromium", "chromium"},
		{"/usr/bin/chromium-browser", "chromium"},
		{"/usr/bin/microsoft-edge-stable", "msedge"},
		{`C:\Program Files\Microsoft\Edge\Application\msedge.exe`, "msedge"},
		{"/snap/bin/brave", "brave"},
		{"/var/lib/flatpak/exports/bin/com.brave.Browser", "brave"},
		{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "chrome"},
		{"/usr/bin/opera", "opera"},
		{"/home/user/tools/my-browser", "unknown"},
	}
	for _, tc := range tests {
		if got := nameFromPath(tc.path); got != tc.want {
			t.Errorf("nameFromPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestDistroInstallCommand(t *testing.T) {
	tests := []struct {
		family string
		want   string
		ok     bool
	}{
		{"debian", "sudo apt install chromium", true},
		{"ubuntu", "sudo apt install chromium", true},
		{"Ubuntu", "sudo apt install chromium", true},
		{"fedora", "sudo dnf install chromium", true},
		{"rhel", "sudo dnf install chromium", true},
		{"arch", "sudo pacman -S chromium", true},
		{"manjaro", "sudo pacman -S chromium", true},
		{"opensuse-leap", "sudo zypper install chromium", true},
		{"alpine", "sudo apk add chromium", true},
		{"nixos", "", false},
		{"windows", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		cmd, ok := distroInstallCommand(tc.family)
		if cmd != tc.want || ok != tc.ok {
			t.Errorf("distroInstallCommand(%q) = (%q, %v), want (%q, %v)", tc.family, cmd, ok, tc.want, tc.ok)
		}
		if strings.ContainsAny(cmd, `/\`) {
			t.Errorf("distroInstallCommand(%q) = %q contains a path separator", tc.family, cmd)
		}
	}
}

func TestInstallGuidanceIsPlainLanguageWithoutPaths(t *testing.T) {
	g := InstallGuidance()
	if strings.TrimSpace(g) == "" {
		t.Fatal("InstallGuidance returned an empty string")
	}
	if strings.ContainsAny(g, `/\`) {
		t.Fatalf("InstallGuidance contains a path separator (§21.4/§22.4): %q", g)
	}
	if !strings.Contains(strings.ToLower(g), "browser") {
		t.Errorf("InstallGuidance does not name the missing component: %q", g)
	}
}
