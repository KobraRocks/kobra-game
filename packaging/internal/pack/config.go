package pack

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config mirrors pkg.toml, the single declared input to the pipeline
// (Packaging spec §14.1). Everything else the build needs is derived from the
// tree and then checked against these declarations.
type Config struct {
	Package   PackageConfig             `toml:"package"`
	Release   ReleaseConfig             `toml:"release"`
	Launcher  LauncherConfig            `toml:"launcher"`
	Platforms map[string]PlatformConfig `toml:"platforms"`
	Publish   PublishConfig             `toml:"publish"`

	// Dir is the directory holding pkg.toml. Relative paths in the config
	// resolve against it, never against the process working directory, so the
	// tool behaves identically from a CI runner and from a shell.
	Dir string `toml:"-"`
}

type PackageConfig struct {
	Slug     string `toml:"slug"`
	GameID   string `toml:"game_id"`
	GameName string `toml:"game_name"`
	PkgRoot  string `toml:"pkgroot"`
}

// ReleaseConfig carries the four independent version axes of §4.4 plus the
// publication facts. They are declared, never inferred: §4.4 requirement 5
// makes a silently-invented game_version a build failure.
type ReleaseConfig struct {
	Release         string `toml:"release"`
	GameVersion     string `toml:"game_version"`
	EngineVersion   string `toml:"engine_version"`
	SaveVersion     int    `toml:"save_version"`
	LauncherMin     string `toml:"launcher_min"`
	Channel         string `toml:"channel"`
	Scope           string `toml:"scope"`
	PreviousRelease string `toml:"previous_release"`
}

type LauncherConfig struct {
	Config        string `toml:"config"`
	DenyList      string `toml:"deny_list"`
	BinaryDir     string `toml:"binary_dir"`
	VersionSource string `toml:"version_source"`
}

type PlatformConfig struct {
	GOOS            string `toml:"goos"`
	GOARCH          string `toml:"goarch"`
	Binary          string `toml:"binary"`
	ArchivePlatform string `toml:"archive_platform"`
}

type PublishConfig struct {
	Base   string `toml:"base"`
	GPGKey string `toml:"gpg_key"`
}

var (
	releaseRe   = regexp.MustCompile(`^[0-9]{4}\.[0-9]{2}\.[0-9]+$`)
	semverRe    = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	gameIDRe    = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9-]+)+$`)
	slugRe      = regexp.MustCompile(`^[a-z0-9-]+$`)
	scopeValues = map[string]bool{"full": true, "game-only": true, "patch": true}
	chanValues  = map[string]bool{"stable": true, "beta": true, "rc": true}
)

// ReleaseSpec is one release the pipeline will build: the declared identity plus
// the overlay directory that carries what changed since the base tree.
type ReleaseSpec struct {
	ReleaseConfig

	// Overlay is the directory applied on top of pkgroot/game, or "" for the
	// base release declared in pkg.toml.
	Overlay string

	Notes      string
	NotesURL   string
	Generated  string // the fixed `published` timestamp, for determinism (§5.2)
	SourceFile string
}

// LoadConfig reads pkg.toml and resolves every relative path against its own
// directory.
func LoadConfig(path string) (*Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, Fail(EvStart, "the package configuration path could not be resolved", map[string]any{"reason": "bad_path"})
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil, Fail(EvStart, "pkg.toml could not be read", map[string]any{"reason": "unreadable"})
	}

	var cfg Config
	if err := toml.Unmarshal(raw, &cfg); err != nil {
		return nil, Fail(EvStart, "pkg.toml is not valid TOML", map[string]any{"reason": "malformed"})
	}
	cfg.Dir = filepath.Dir(abs)
	return &cfg, nil
}

// Validate applies the §14.1 and §4.x rules a declaration can be checked
// against on its own. Cross-document agreement is §4.6 and lives in CheckIdentity.
func (c *Config) Validate() error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if c.Package.Slug == "" || !slugRe.MatchString(c.Package.Slug) {
		add("package.slug must match %s", slugRe)
	}
	if !gameIDRe.MatchString(c.Package.GameID) {
		add("package.game_id must be reverse-DNS lowercase matching %s", gameIDRe)
	}
	if c.Package.GameName == "" {
		add("package.game_name is required")
	}
	if last := lastSegment(c.Package.GameID); last != c.Package.Slug {
		add("package.slug %q must equal the last segment of game_id (%q)", c.Package.Slug, last)
	}
	if _, err := c.PkgRoot(); err != nil {
		add("package.pkgroot is not a directory")
	}

	if !releaseRe.MatchString(c.Release.Release) {
		add("release.release must match %s", releaseRe)
	}
	if !semverRe.MatchString(c.Release.GameVersion) {
		add("release.game_version must match %s and MUST be declared (§4.4)", semverRe)
	}
	if !semverRe.MatchString(c.Release.EngineVersion) {
		add("release.engine_version must match %s", semverRe)
	}
	if !semverRe.MatchString(c.Release.LauncherMin) {
		add("release.launcher_min must match %s", semverRe)
	}
	if c.Release.SaveVersion < 1 {
		add("release.save_version must be >= 1")
	}
	if c.Release.Channel != "" && !chanValues[c.Release.Channel] {
		add("release.channel must be one of stable, beta, rc")
	}
	if c.Release.Scope != "" && !scopeValues[c.Release.Scope] {
		add("release.scope must be one of full, game-only, patch")
	}

	if len(c.Platforms) == 0 {
		add("at least one [platforms.<name>] table is required")
	}
	for name, p := range c.Platforms {
		if p.GOOS == "" || p.GOARCH == "" || p.Binary == "" {
			add("platforms.%s needs goos, goarch and binary", name)
		}
		if p.ArchivePlatform == "" {
			add("platforms.%s needs archive_platform (§3.2)", name)
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return Fail(EvIdentityFail, strings.Join(problems, "; "),
			map[string]any{"check": "config", "count": len(problems)})
	}
	return nil
}

// PkgRoot is the resolved directory containing game/.
func (c *Config) PkgRoot() (string, error) {
	root := c.Package.PkgRoot
	if root == "" {
		root = "."
	}
	if !filepath.IsAbs(root) {
		root = filepath.Join(c.Dir, root)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("pkgroot is not a directory")
	}
	return abs, nil
}

// DefaultPlatform picks the platform to build when none is named. Linux x64 is
// the project's first-class target; any other set falls back to the first
// platform in sorted order so the choice is still deterministic.
func (c *Config) DefaultPlatform() string {
	if _, ok := c.Platforms["linux-x64"]; ok {
		return "linux-x64"
	}
	names := make([]string, 0, len(c.Platforms))
	for name := range c.Platforms {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// ResolvePath resolves a config-relative path.
func (c *Config) ResolvePath(rel string) string {
	if rel == "" {
		return ""
	}
	if filepath.IsAbs(rel) {
		return rel
	}
	return filepath.Join(c.Dir, rel)
}

// LauncherBinary returns the launcher binary for a platform, preferring the
// per-platform build directory and falling back to a direct path for the
// platform's own build.
func (c *Config) LauncherBinary(name string, platform PlatformConfig, override string) (string, error) {
	if override != "" {
		if st, err := os.Stat(override); err == nil && !st.IsDir() {
			return override, nil
		}
		return "", fmt.Errorf("--launcher %s is not a file", override)
	}
	dir := c.ResolvePath(c.Launcher.BinaryDir)
	if dir == "" {
		dir = filepath.Join(c.Dir, "dist")
	}
	candidates := []string{
		filepath.Join(dir, platform.GOOS+"-"+platform.GOARCH, platform.Binary),
		filepath.Join(dir, platform.Binary),
	}
	for _, candidate := range candidates {
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf(
		"no launcher binary for %s; expected %s (build it with `make -C launcher cross`)",
		name, candidates[0])
}

// LoadReleases returns the base release declared in pkg.toml plus every
// releases/<id>/release.toml overlay, ordered by release id.
//
// The overlay convention is the one testgame/releases/README.md documents: a
// later release stores only what changed, so the fixture stays readable and a
// studio's repo does not carry N copies of the tree.
func (c *Config) LoadReleases() ([]ReleaseSpec, error) {
	base := ReleaseSpec{
		ReleaseConfig: c.Release,
		SourceFile:    "pkg.toml",
	}
	// A declared publication time keeps archives byte-identical across runs
	// (§5.2); it is the release's own timestamp, never the build clock.
	base.Generated = fixedTimestamp(c.Release.Release)

	specs := []ReleaseSpec{base}

	dir := filepath.Join(c.Dir, "releases")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return specs, nil
		}
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		file := filepath.Join(dir, entry.Name(), "release.toml")
		raw, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		var doc struct {
			Release struct {
				Release         string `toml:"release"`
				GameVersion     string `toml:"game_version"`
				EngineVersion   string `toml:"engine_version"`
				SaveVersion     *int   `toml:"save_version"`
				LauncherMin     string `toml:"launcher_min"`
				Channel         string `toml:"channel"`
				Scope           string `toml:"scope"`
				PreviousRelease string `toml:"previous_release"`
			} `toml:"release"`
			Notes struct {
				Summary string `toml:"summary"`
				URL     string `toml:"url"`
			} `toml:"notes"`
		}
		if err := toml.Unmarshal(raw, &doc); err != nil {
			return nil, Fail(EvStart, "a release overlay is not valid TOML",
				map[string]any{"file": entry.Name(), "reason": "malformed"})
		}
		if doc.Release.Release == "" {
			return nil, Fail(EvStart, "a release overlay declares no release id",
				map[string]any{"file": entry.Name()})
		}
		if doc.Release.Release != entry.Name() {
			return nil, Fail(EvStart, "a release overlay's directory name must equal its release id",
				map[string]any{"dir": entry.Name(), "release": doc.Release.Release})
		}

		spec := ReleaseSpec{
			ReleaseConfig: c.Release, // inherit, then override what is declared
			Overlay:       filepath.Join(dir, entry.Name(), "overlay"),
			Notes:         doc.Notes.Summary,
			NotesURL:      doc.Notes.URL,
			Generated:     fixedTimestamp(doc.Release.Release),
			SourceFile:    filepath.Join("releases", entry.Name(), "release.toml"),
		}
		spec.Release = doc.Release.Release
		if doc.Release.GameVersion != "" {
			spec.GameVersion = doc.Release.GameVersion
		}
		if doc.Release.EngineVersion != "" {
			spec.EngineVersion = doc.Release.EngineVersion
		}
		if doc.Release.SaveVersion != nil {
			spec.SaveVersion = *doc.Release.SaveVersion
		}
		if doc.Release.LauncherMin != "" {
			spec.LauncherMin = doc.Release.LauncherMin
		}
		if doc.Release.Channel != "" {
			spec.Channel = doc.Release.Channel
		}
		if doc.Release.Scope != "" {
			spec.Scope = doc.Release.Scope
		}
		spec.PreviousRelease = doc.Release.PreviousRelease

		if !releaseRe.MatchString(spec.Release) {
			return nil, Fail(EvIdentityFail, "a release id does not match the calver pattern",
				map[string]any{"release": spec.Release, "check": "release_pattern"})
		}
		if spec.Scope == "patch" && spec.PreviousRelease == "" {
			return nil, Fail(EvIdentityFail, "a patch release must declare previous_release (§9.2)",
				map[string]any{"release": spec.Release, "check": "patch_previous"})
		}
		specs = append(specs, spec)
	}

	sort.Slice(specs, func(i, j int) bool {
		return compareReleases(specs[i].Release, specs[j].Release) < 0
	})
	return specs, nil
}

// compareReleases orders calver ids by parsing the segments as integers.
// §13.3 warns that the schema's lexicographic rule is unsound for the counter,
// so tooling that must order releases must not rely on it.
func compareReleases(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		ai, _ := strconv.Atoi(as[i])
		bi, _ := strconv.Atoi(bs[i])
		if ai != bi {
			if ai < bi {
				return -1
			}
			return 1
		}
	}
	return len(as) - len(bs)
}

// fixedTimestamp derives a stable `published` value from the release id so that
// two builds of one release are byte-identical (§5.2). The year and month come
// from the id; the day is fixed at 01 and the counter picks the hour, which
// keeps the value unique per release without reading the clock.
func fixedTimestamp(release string) string {
	parts := strings.Split(release, ".")
	if len(parts) != 3 {
		return "1970-01-01T00:00:00Z"
	}
	hour, _ := strconv.Atoi(parts[2])
	return fmt.Sprintf("%s-%s-01T%02d:00:00Z", parts[0], parts[1], hour%24)
}

func lastSegment(id string) string {
	if i := strings.LastIndexByte(id, '.'); i >= 0 {
		return id[i+1:]
	}
	return id
}

// CompareSemver orders two bare x.y.z versions, returning -1, 0 or +1.
//
// ok is false when either argument is not a bare three-component numeric version,
// and the caller MUST then fail its gate closed. Versions in this project carry no
// prerelease or build-metadata suffix — pre-release status is the release
// manifest's `channel` — so a suffix is invalid input, not a lower version
// (VERSIONING.md). Coercing an unparseable component to zero, which this function
// used to do, made "1.0.0-rc.1" compare equal to "1.0.0" and silently pass a gate
// the launcher refuses at run time.
func CompareSemver(a, b string) (int, bool) {
	as, aok := splitSemver(a)
	bs, bok := splitSemver(b)
	if !aok || !bok {
		return 0, false
	}
	for i := 0; i < 3; i++ {
		if as[i] != bs[i] {
			if as[i] < bs[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// splitSemver parses a bare x.y.z version into its three components. It reports
// ok=false for anything else: a missing or extra component, an empty component, a
// non-numeric component, a leading "v", or any prerelease or build suffix.
func splitSemver(s string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		if p == "" {
			return out, false
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
