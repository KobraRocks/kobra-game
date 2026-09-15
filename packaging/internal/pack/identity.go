package pack

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var bareSemver = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// LauncherVersion reads the version out of a launcher binary with --version.
//
// §4.6 requires launcher/VERSION to be generated from the binary rather than
// hand-edited, so the binary is the authority. A release build prints a bare
// semver; a bare `go build` leaves the default 0.1.0-dev, which is not a valid
// version to ship and is reported as such rather than silently accepted.
func LauncherVersion(binary string) (string, error) {
	out, err := exec.Command(binary, "--version").Output()
	if err != nil {
		return "", Fail(EvIdentityFail, "the launcher binary did not answer --version",
			map[string]any{"check": "launcher_version", "reason": "exec"})
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) < 2 || !bareSemver.MatchString(fields[1]) {
		return "", Fail(EvIdentityFail,
			"the launcher binary does not print a bare semver; build it with `make -C launcher build`",
			map[string]any{"check": "launcher_version", "reason": "unstamped"})
	}
	return fields[1], nil
}

// IdentityInput is everything §4.6 cross-checks.
type IdentityInput struct {
	Config         *Config
	Spec           ReleaseSpec
	Engine         *EngineManifest
	LauncherConfig map[string]any
	LauncherVer    string
	Stage          string
	GameName       string
}

// CheckIdentity implements §4.6. Every disagreement is reported, not just the
// first, because a publisher fixing one at a time is a slow loop.
func CheckIdentity(in IdentityInput, log *Logger) error {
	type failure struct {
		check    string
		expected string
		actual   string
	}
	var failures []failure
	check := func(name, expected, actual string) {
		if expected != actual {
			failures = append(failures, failure{name, expected, actual})
		}
	}

	gameDir := filepath.Join(in.Stage, in.GameName, "game")
	release := in.Spec.Release

	check("engine manifest release", release, in.Engine.Release)
	check("launcher config release", release, str(in.LauncherConfig["release"]))
	check("engine_version agreement", in.Spec.EngineVersion, in.Engine.EngineVersion)
	check("save_version agreement", fmt.Sprint(in.Spec.SaveVersion), fmt.Sprint(in.Engine.SaveVersion))
	check("game_version declared", "yes", yesNo(in.Spec.GameVersion != ""))
	check("game_version well formed", "yes", yesNo(bareSemver.MatchString(in.Spec.GameVersion)))
	check("launcher_min <= launcher", "yes",
		yesNo(in.LauncherVer != "" && CompareSemver(in.Spec.LauncherMin, in.LauncherVer) <= 0))
	check("slug matches game_id", lastSegment(in.Config.Package.GameID), in.Config.Package.Slug)

	// §5.4: the engine's declared files must exist, and .wasm must be served as
	// application/wasm or instantiateStreaming fails in the browser (FR-AST-2).
	for _, key := range []string{in.Engine.Wasm, in.Engine.Glue} {
		if key == "" {
			failures = append(failures, failure{"engine file declared", "a path", "nothing"})
			continue
		}
		full := filepath.Join(gameDir, filepath.FromSlash(key))
		if _, err := os.Stat(full); err != nil {
			failures = append(failures, failure{"engine file exists", key, "missing"})
		}
	}
	check("wasm served as application/wasm", "application/wasm", wasmMIME(in.LauncherConfig))

	if len(failures) > 0 {
		for _, f := range failures {
			log.Error(EvIdentityFail, map[string]any{
				"check": f.check, "expected": f.expected, "actual": f.actual,
			})
		}
		return Fail(EvIdentityFail, fmt.Sprintf("%d identity check(s) failed", len(failures)),
			map[string]any{"count": len(failures)})
	}

	log.Info(EvIdentityOK, map[string]any{
		"release":        release,
		"game_version":   in.Spec.GameVersion,
		"engine_version": in.Engine.EngineVersion,
		"save_version":   in.Spec.SaveVersion,
		"launcher_min":   in.Spec.LauncherMin,
		"launcher":       in.LauncherVer,
	})
	return nil
}

// wasmMIME resolves the media type the packaged config will serve .wasm with.
// An absent entry falls back to the launcher's default table.
func wasmMIME(cfg map[string]any) string {
	table, ok := cfg["mime_types"].(map[string]any)
	if !ok {
		return "application/wasm"
	}
	if value, ok := table[".wasm"].(string); ok && value != "" {
		return value
	}
	return "application/wasm"
}

func yesNo(ok bool) string {
	if ok {
		return "yes"
	}
	return "no"
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
