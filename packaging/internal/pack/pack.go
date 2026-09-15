package pack

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// ManifestRel is the path of the release manifest inside a package. It is the
// one path that must never appear in an update archive.
const ManifestRel = "game/release.manifest.json"

// Options selects what a build does. Everything here is a declared choice: the
// pipeline never infers a release id, a platform or a signing key.
type Options struct {
	ConfigPath string
	OutDir     string
	Platform   string
	Launcher   string // override the binary path, for a dev loop
	Releases   []string
	Install    bool
	Sign       bool
}

// Result reports what a build produced.
type Result struct {
	OutDir      string
	Releases    []string
	Published   []string
	InstallPath string
}

// Pipeline is one configured packaging run.
type Pipeline struct {
	cfg       *Config
	log       *Logger
	validator *Validator
}

// NewPipeline builds a pipeline and compiles the schemas once.
func NewPipeline(cfg *Config, log *Logger) (*Pipeline, error) {
	validator, err := NewValidator(log)
	if err != nil {
		return nil, err
	}
	return &Pipeline{cfg: cfg, log: log, validator: validator}, nil
}

// prepared is everything resolved once per run, so `check` and `build` cannot
// drift apart in how they resolve the same inputs.
type prepared struct {
	pkgRoot         string
	platformName    string
	platform        PlatformConfig
	launcherBinary  string
	launcherVersion string
	denyList        string
	baseConfig      map[string]any
	releases        []ReleaseSpec
	gameName        string
	outDir          string
}

func (p *Pipeline) prepare(opts Options) (*prepared, error) {
	if err := p.cfg.Validate(); err != nil {
		return nil, err
	}
	pkgRoot, err := p.cfg.PkgRoot()
	if err != nil {
		return nil, Fail(EvStart, "pkgroot could not be resolved", map[string]any{"reason": "pkgroot"})
	}

	platformName := opts.Platform
	if platformName == "" {
		platformName = p.cfg.DefaultPlatform()
	}
	platform, ok := p.cfg.Platforms[platformName]
	if !ok {
		names := make([]string, 0, len(p.cfg.Platforms))
		for name := range p.cfg.Platforms {
			names = append(names, name)
		}
		sort.Strings(names)
		return nil, Fail(EvStart, "unknown platform",
			map[string]any{"platform": platformName, "known": strings.Join(names, ",")})
	}

	launcherBinary, err := p.cfg.LauncherBinary(platformName, platform, opts.Launcher)
	if err != nil {
		return nil, Fail(EvStart, err.Error(), map[string]any{"reason": "no_launcher"})
	}
	launcherVersion, err := LauncherVersion(launcherBinary)
	if err != nil {
		return nil, err
	}

	denyList := p.cfg.ResolvePath(p.cfg.Launcher.DenyList)
	if _, err := os.Stat(denyList); err != nil {
		return nil, Fail(EvStart, "the shared port deny list is missing", map[string]any{"reason": "no_deny_list"})
	}

	configTemplate := p.cfg.ResolvePath(p.cfg.Launcher.Config)
	if configTemplate == "" {
		configTemplate = filepath.Join(pkgRoot, "launcher", "launcher.config.json")
	}
	var baseConfig map[string]any
	if err := ReadJSON(configTemplate, &baseConfig); err != nil {
		return nil, Fail(EvStart, "launcher/launcher.config.json could not be read",
			map[string]any{"reason": "no_launcher_config"})
	}

	releases, err := p.cfg.LoadReleases()
	if err != nil {
		return nil, err
	}
	if len(opts.Releases) > 0 {
		want := map[string]bool{}
		for _, r := range opts.Releases {
			want[r] = true
		}
		var filtered []ReleaseSpec
		for _, spec := range releases {
			if want[spec.Release] {
				filtered = append(filtered, spec)
			}
		}
		if len(filtered) == 0 {
			return nil, Fail(EvStart, "no release matched --release", map[string]any{"reason": "no_release"})
		}
		releases = filtered
	}

	outDir := opts.OutDir
	if outDir == "" {
		outDir = filepath.Join(p.cfg.Dir, "dist")
	}

	return &prepared{
		pkgRoot:         pkgRoot,
		platformName:    platformName,
		platform:        platform,
		launcherBinary:  launcherBinary,
		launcherVersion: launcherVersion,
		denyList:        denyList,
		baseConfig:      baseConfig,
		releases:        releases,
		gameName:        gameFolderName(p.cfg.Package.Slug),
		outDir:          outDir,
	}, nil
}

// Check runs every gate that does not require writing an archive, and writes
// nothing.
//
// A studio's pre-commit hook needs this: validating a multi-gigabyte tree must
// not mean building a multi-gigabyte archive.
func (p *Pipeline) Check(opts Options) error {
	prep, err := p.prepare(opts)
	if err != nil {
		return err
	}
	scratch, err := os.MkdirTemp("", "kobra-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)

	for _, spec := range prep.releases {
		stage := filepath.Join(scratch, spec.Release)
		if err := os.MkdirAll(stage, 0o755); err != nil {
			return err
		}
		if _, err := p.stageRelease(spec, stage, prep); err != nil {
			return err
		}
	}
	p.log.Info(EvCheckOK, map[string]any{"releases": len(prep.releases), "platform": prep.platformName})
	return nil
}

// Run builds every selected release into OutDir.
func (p *Pipeline) Run(opts Options) (*Result, error) {
	prep, err := p.prepare(opts)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(prep.outDir, 0o755); err != nil {
		return nil, err
	}

	scratch, err := os.MkdirTemp("", "kobra-pack-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)

	result := &Result{OutDir: prep.outDir}
	var published []string
	var latest Latest
	var installSource string

	for _, spec := range prep.releases {
		p.log.Info(EvStart, map[string]any{
			"release": spec.Release, "platform": prep.platformName, "pkgroot_kind": "directory",
		})
		stage := filepath.Join(scratch, spec.Release)
		if err := os.MkdirAll(stage, 0o755); err != nil {
			return nil, err
		}

		staged, err := p.stageRelease(spec, stage, prep)
		if err != nil {
			return nil, err
		}

		base := fmt.Sprintf("%s-%s-%s", p.cfg.Package.Slug, spec.Release, prep.platform.ArchivePlatform)
		zipPath := filepath.Join(prep.outDir, base+".zip")
		tarPath := filepath.Join(prep.outDir, base+".tar.zst")
		manifestPath := filepath.Join(prep.outDir,
			fmt.Sprintf("%s-%s.release.manifest.json", p.cfg.Package.Slug, spec.Release))

		// Stage 8 before stages 5-7: the manifest declares the archive's hash,
		// so the archive must exist first, and the manifest is never a member
		// of it (§1.2).
		if err := BuildUpdateArchive(stage, prep.gameName, ManifestRel, tarPath,
			ArchiveTime(spec.Generated), p.log); err != nil {
			return nil, err
		}
		tarHash, tarSize, err := HashFile(tarPath)
		if err != nil {
			return nil, err
		}

		manifest, err := GenerateReleaseManifest(stage, prep.gameName, ReleaseManifestParams{
			Spec:          spec,
			LauncherMin:   spec.LauncherMin,
			EngineVersion: staged.engine.EngineVersion,
			SaveVersion:   staged.engine.SaveVersion,
			GameVersion:   spec.GameVersion,
			Archive: &ArchiveRef{
				Name: filepath.Base(tarPath), URL: filepath.Base(tarPath),
				Size: tarSize, Hash: tarHash, Format: "tar.zst",
			},
		}, staged.index, staged.totalSize, p.log)
		if err != nil {
			return nil, err
		}
		if err := p.validator.Validate(SchemaReleaseManifest, manifest); err != nil {
			p.log.Error(EvSchemaFail, map[string]any{"schema": SchemaReleaseManifest})
			return nil, err
		}
		if err := CheckServable(stage, prep.gameName); err != nil {
			emitScan(err, p.log)
			return nil, err
		}

		// Stage 7: the ZIP. README.txt states the ZIP's own download size, so
		// the pair is iterated to a fixpoint. Storing README.txt uncompressed
		// makes its contribution exactly its length, so it settles at once.
		//
		// README.txt's "Extracted size" is what a user gets when they unzip the
		// package, so it counts the whole archive, not only the release index:
		// the index sums game/ and launcher/ but the ZIP also carries the
		// release manifest, README.txt and LICENSES/README.txt (data/.keep is
		// zero bytes). The manifest's own total_size stays the index sum,
		// because that is the payload the launcher checks the update archive
		// against (schema release.manifest.json, archive section).
		manifestSize := fileSize(filepath.Join(stage, prep.gameName, filepath.FromSlash(ManifestRel)))
		extracted := staged.totalSize + manifestSize + readmeSize(prep.gameName) + int64(len(licenceNotice))
		sizes := Sizes{Download: 0, Extracted: extracted, Recommended: extracted * 3}
		settled := false
		for attempt := 0; attempt < 8; attempt++ {
			if err := BuildExtraFiles(stage, prep.gameName, sizes); err != nil {
				return nil, err
			}
			if err := BuildZip(stage, prep.gameName, zipPath, ArchiveTime(spec.Generated), p.log); err != nil {
				return nil, err
			}
			actual := fileSize(zipPath)
			if actual == sizes.Download {
				settled = true
				break
			}
			sizes.Download = actual
		}
		if !settled {
			p.log.Warn(EvReadmeUnstable, map[string]any{"reason": "zip size did not settle in 8 passes"})
		}

		// One manifest, two byte-identical copies: inside the ZIP for support,
		// and at the base URL where the launcher fetches it.
		if err := copyFile(filepath.Join(stage, prep.gameName, filepath.FromSlash(ManifestRel)), manifestPath); err != nil {
			return nil, Fail(EvPublish, "the release manifest could not be published", map[string]any{"reason": "copy"})
		}
		p.log.Info(EvPublish, map[string]any{"base": prep.outDir, "release": spec.Release, "files": 3})

		published = append(published,
			filepath.Base(zipPath), filepath.Base(tarPath), filepath.Base(manifestPath))
		if installSource == "" {
			installSource = filepath.Base(zipPath)
		}
		latest = Latest{
			Release:     spec.Release,
			ManifestURL: filepath.Base(manifestPath),
			NotesURL:    spec.NotesURL,
		}
		result.Releases = append(result.Releases, spec.Release)
	}

	// §8.4: latest.json is written last and is the rollback lever.
	if err := WriteLatest(prep.outDir, latest); err != nil {
		return nil, err
	}
	published = append(published, "latest.json")

	if err := WriteSha256Sums(prep.outDir, published, p.log); err != nil {
		return nil, err
	}
	if opts.Sign {
		if err := p.signSums(prep.outDir); err != nil {
			return nil, err
		}
	} else {
		p.log.Warn(EvSignSkipped, map[string]any{"kind": "gpg", "reason": "not_requested"})
	}

	if err := VerifyOutput(prep.outDir, published, p.log); err != nil {
		return nil, err
	}

	if opts.Install && installSource != "" {
		root, err := ExtractZip(filepath.Join(prep.outDir, installSource), filepath.Join(prep.outDir, "install"))
		if err != nil {
			return nil, Fail(EvPublish, "the built package could not be extracted", map[string]any{"reason": "install"})
		}
		result.InstallPath = root
		p.log.Info(EvPublish, map[string]any{"base": root, "files": len(mustRelFiles(root))})
	}

	result.Published = published
	p.log.Info(EvDone, map[string]any{"releases": len(result.Releases), "out": prep.outDir})
	return result, nil
}

// stagedRelease is what stages 3-5 produce: a tree ready to be archived, with
// the file index already computed from it.
type stagedRelease struct {
	engine    *EngineManifest
	index     []FileEntry
	totalSize int64
}

// stageRelease runs §5.1 stages 3-5 plus every pre-archive gate: collect, scan,
// identity, schema and servability. Both `check` and `build` go through here,
// so a gate cannot exist in one and not the other.
func (p *Pipeline) stageRelease(spec ReleaseSpec, stage string, prep *prepared) (*stagedRelease, error) {
	if err := CollectTree(prep.pkgRoot, spec.Overlay, stage, prep.gameName); err != nil {
		return nil, err
	}

	// Assemble launcher/ (§2.3). The config is rendered per release so its
	// `release` field tracks the release id, as §4.6 requires.
	launcherCfg := make(map[string]any, len(prep.baseConfig))
	for k, v := range prep.baseConfig {
		launcherCfg[k] = v
	}
	launcherCfg["release"] = spec.Release

	launcherDir := filepath.Join(stage, prep.gameName, "launcher")
	if err := os.MkdirAll(launcherDir, 0o755); err != nil {
		return nil, err
	}
	binaryTarget := filepath.Join(launcherDir, prep.platform.Binary)
	if err := copyFile(prep.launcherBinary, binaryTarget); err != nil {
		return nil, Fail(EvStart, "the launcher binary could not be staged", map[string]any{"reason": "copy"})
	}
	if err := os.Chmod(binaryTarget, 0o755); err != nil {
		return nil, err
	}
	if err := copyFile(prep.denyList, filepath.Join(launcherDir, "port-deny-list.json")); err != nil {
		return nil, Fail(EvStart, "the port deny list could not be staged", map[string]any{"reason": "copy"})
	}
	if err := WriteJSON(filepath.Join(launcherDir, "launcher.config.json"), launcherCfg); err != nil {
		return nil, err
	}
	// §4.6: VERSION is generated from the binary, never hand-edited.
	if err := os.WriteFile(filepath.Join(launcherDir, "VERSION"), []byte(prep.launcherVersion+"\n"), 0o644); err != nil {
		return nil, err
	}

	// The data/ skeleton (§2.5) is created before the scan so the "only .keep
	// under data/" rule is actually exercised.
	if err := BuildDataSkeleton(stage, prep.gameName); err != nil {
		return nil, err
	}
	if err := ScanForbidden(stage, prep.gameName); err != nil {
		emitScan(err, p.log)
		return nil, err
	}

	engine, err := ReadEngineManifest(stage, prep.gameName)
	if err != nil {
		return nil, Fail(EvIdentityFail, "game/engine/engine.manifest.json is missing",
			map[string]any{"check": "engine_manifest"})
	}
	if err := p.validator.Validate(SchemaLauncherConfig, launcherCfg); err != nil {
		p.log.Error(EvSchemaFail, map[string]any{"schema": SchemaLauncherConfig})
		return nil, err
	}
	if err := p.validator.Validate(SchemaEngineManifest, engine); err != nil {
		p.log.Error(EvSchemaFail, map[string]any{"schema": SchemaEngineManifest})
		return nil, err
	}
	if err := CheckIdentity(IdentityInput{
		Config: p.cfg, Spec: spec, Engine: engine, LauncherConfig: launcherCfg,
		LauncherVer: prep.launcherVersion, Stage: stage, GameName: prep.gameName,
	}, p.log); err != nil {
		return nil, err
	}

	assets, err := GenerateAssetManifest(stage, prep.gameName, spec.Release, spec.Generated, p.log)
	if err != nil {
		return nil, err
	}
	if err := p.validator.Validate(SchemaAssetManifest, assets); err != nil {
		p.log.Error(EvSchemaFail, map[string]any{"schema": SchemaAssetManifest})
		return nil, err
	}

	index, totalSize, err := BuildFileIndex(stage, prep.gameName, ManifestRel)
	if err != nil {
		return nil, err
	}
	if err := CheckServable(stage, prep.gameName); err != nil {
		emitScan(err, p.log)
		return nil, err
	}

	// Validate the release manifest document here, not only in Run: this is
	// where §6.4's whole-tree path pattern is enforced, and `check` must run the
	// same gate as `build`. `archive` is nil because no archive exists yet; the
	// schema does not require it, and Run validates the document again once the
	// archive hash is known.
	manifestDoc := releaseManifestDoc(ReleaseManifestParams{
		Spec:          spec,
		LauncherMin:   spec.LauncherMin,
		EngineVersion: engine.EngineVersion,
		SaveVersion:   engine.SaveVersion,
		GameVersion:   spec.GameVersion,
	}, index, totalSize)
	if err := p.validator.Validate(SchemaReleaseManifest, manifestDoc); err != nil {
		p.log.Error(EvSchemaFail, map[string]any{"schema": SchemaReleaseManifest})
		return nil, err
	}

	return &stagedRelease{engine: engine, index: index, totalSize: totalSize}, nil
}

// signSums produces SHA256SUMS.sig when a signing key is configured (§7.1).
//
// Only the checksum file is signed. The manifest's `signatures` object is
// deliberately not populated: §7.4 requires the signature to cover the manifest
// document "exactly as published", and a signature stored inside that document
// cannot cover it. Resolving that needs a stated canonical form (the manifest
// with `signatures` removed), which is a specification decision, not a build
// decision.
func (p *Pipeline) signSums(outDir string) error {
	key := p.cfg.Publish.GPGKey
	if key == "" {
		p.log.Warn(EvSignSkipped, map[string]any{"kind": "gpg", "reason": "no_key_configured"})
		return nil
	}
	if _, err := exec.LookPath("gpg"); err != nil {
		p.log.Warn(EvSignSkipped, map[string]any{"kind": "gpg", "reason": "gpg_not_installed"})
		return nil
	}
	cmd := exec.Command("gpg", "--batch", "--yes", "--armor",
		"--local-user", key, "--detach-sign",
		"--output", filepath.Join(outDir, "SHA256SUMS.sig"),
		filepath.Join(outDir, "SHA256SUMS"))
	if out, err := cmd.CombinedOutput(); err != nil {
		return Fail(EvSign, "SHA256SUMS could not be signed",
			map[string]any{"reason": "gpg_failed", "detail": firstLine(string(out))})
	}
	p.log.Info(EvSign, map[string]any{"kind": "gpg", "key_fingerprint": strings.ToUpper(key)})
	return nil
}

func emitScan(err error, log *Logger) {
	if scan, ok := err.(*ScanErrors); ok {
		scan.Emit(log)
	}
}

// gameFolderName is the single top-level directory inside the ZIP (§3.1).
func gameFolderName(slug string) string {
	parts := strings.FieldsFunc(slug, func(r rune) bool { return r == '-' || r == '_' })
	var b strings.Builder
	for _, part := range parts {
		if part == "" {
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]))
		b.WriteString(part[1:])
	}
	if b.Len() == 0 {
		return "Game"
	}
	return b.String()
}

func mustRelFiles(root string) []string {
	files, err := RelFiles(root)
	if err != nil {
		return nil
	}
	return files
}
