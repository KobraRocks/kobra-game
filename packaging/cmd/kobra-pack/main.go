// Command kobra-pack builds a Kobra game package from a development tree.
//
// It is the publisher's tool: it runs on a studio's machine and in the studio's
// CI, and nothing it produces is ever shipped as part of the launcher. See
// packaging/README.md for the studio workflow and
// architecture/Packaging-spec.md for the rules it enforces.
//
//	kobra-pack check                 run every gate, write nothing
//	kobra-pack build                 produce a publishable dist/
//	kobra-pack dev                   serve the source tree with the launcher
//	kobra-pack verify <dir>          re-verify a built dist/
//	kobra-pack version               print the tool version
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"kobragames.local/packaging/internal/pack"
)

// version is stamped at build time; a source build reports dev.
var version = "0.1.0-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	command := os.Args[1]
	args := os.Args[2:]

	var err error
	switch command {
	case "check":
		err = runCheck(args)
	case "build":
		err = runBuild(args)
	case "dev":
		err = runDev(args)
	case "verify":
		err = runVerify(args)
	case "version", "--version", "-version":
		fmt.Printf("kobra-pack %s\n", version)
		return
	case "help", "--help", "-h":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "kobra-pack: unknown command %q\n\n", command)
		usage()
		os.Exit(2)
	}

	if err != nil {
		var build *pack.BuildError
		if errors.As(err, &build) {
			// The structured event already carried the detail; this line is the
			// human summary a CI log wants at the end.
			fmt.Fprintf(os.Stderr, "\npackaging failed: %s\n", build.Msg)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "\npackaging failed: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `kobra-pack — build a Kobra game package and verify it

USAGE
  kobra-pack <command> [flags]

COMMANDS
  check     run every build gate and write nothing (pre-commit, CI)
  build     produce the publishable artefacts in dist/
  dev       assemble a shim folder and run the launcher against the source tree
  verify    re-verify a built dist/ without the launcher
  version   print the tool version

COMMON FLAGS
  --config PATH      pkg.toml to read (default: ./pkg.toml)
  --platform NAME    platform from [platforms.*] (default: linux-x64)
  --release ID       build only this release id; repeatable
  --launcher PATH    use this launcher binary instead of the configured one
  --json             emit the event stream as one JSON object per line

BUILD FLAGS
  --out DIR          output directory (default: ./dist)
  --install          also extract the first release into <out>/install/
  --sign             sign SHA256SUMS with the configured GPG key

DEV FLAGS
  --open             let the launcher open a browser (default: do not)
  --port N           pin the launcher port
  --data-dir PATH    keep saves outside the source tree (recommended)
`)
}

// common holds the flags every command accepts.
type common struct {
	config   string
	platform string
	launcher string
	jsonLog  bool
	releases multiFlag
}

func addCommon(fs *flag.FlagSet) *common {
	c := &common{}
	fs.StringVar(&c.config, "config", "pkg.toml", "pkg.toml to read")
	fs.StringVar(&c.platform, "platform", "", "platform from [platforms.*]")
	fs.StringVar(&c.launcher, "launcher", "", "launcher binary to package")
	fs.BoolVar(&c.jsonLog, "json", false, "emit the event stream as JSON lines")
	fs.Var(&c.releases, "release", "build only this release id (repeatable)")
	return c
}

func (c *common) options(outDir string, install, sign bool) pack.Options {
	return pack.Options{
		ConfigPath: c.config,
		OutDir:     outDir,
		Platform:   c.platform,
		Launcher:   c.launcher,
		Releases:   c.releases,
		Install:    install,
		Sign:       sign,
	}
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// newPipeline loads pkg.toml and builds the pipeline both commands share.
func newPipeline(c *common) (*pack.Pipeline, error) {
	logger := pack.NewLogger(os.Stderr, c.jsonLog)
	cfg, err := pack.LoadConfig(c.config)
	if err != nil {
		logger.Error("pack.start", map[string]any{"reason": "config"})
		return nil, err
	}
	// Name the resolved roots once, so a CI log shows what was actually read.
	logger.Info("pack.start", map[string]any{
		"config":   abs(c.config),
		"pkgroot":  mustPkgRoot(cfg),
		"launcher": c.launcher,
		"platform": c.platform,
		"tool":     version,
	})
	return pack.NewPipeline(cfg, logger)
}

func mustPkgRoot(cfg *pack.Config) string {
	root, err := cfg.PkgRoot()
	if err != nil {
		return "?"
	}
	return root
}

func runCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	c := addCommon(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	pipeline, err := newPipeline(c)
	if err != nil {
		return err
	}
	return pipeline.Check(c.options("", false, false))
}

func runBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	c := addCommon(fs)
	outDir := fs.String("out", "", "output directory (default: ./dist)")
	install := fs.Bool("install", false, "extract the first release into <out>/install/")
	sign := fs.Bool("sign", false, "sign SHA256SUMS with the configured GPG key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *outDir == "" {
		*outDir = filepath.Join(filepath.Dir(abs(c.config)), "dist")
	}
	pipeline, err := newPipeline(c)
	if err != nil {
		return err
	}
	result, err := pipeline.Run(c.options(*outDir, *install, *sign))
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "\nbuilt %s\n", result.OutDir)
	for _, name := range result.Published {
		fmt.Fprintf(os.Stderr, "  %s\n", name)
	}
	if result.InstallPath != "" {
		fmt.Fprintf(os.Stderr, "  install: %s\n", result.InstallPath)
	}
	return nil
}

// runDev assembles a shim game folder whose game/ is a symlink to the source
// tree and runs the launcher against it.
//
// The launcher resolves its game folder from its own executable path and never
// from the working directory (FR-LNCH-1), so a developer cannot point it at a
// source tree directly. The shim satisfies the layout without copying anything,
// and data/ is a real directory under the shim so saves never land in the source
// tree.
func runDev(args []string) error {
	fs := flag.NewFlagSet("dev", flag.ExitOnError)
	c := addCommon(fs)
	open := fs.Bool("open", false, "let the launcher open a browser")
	port := fs.Int("port", 0, "pin the launcher port")
	dataDir := fs.String("data-dir", "", "keep saves outside the source tree")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pipeline, err := newPipeline(c)
	if err != nil {
		return err
	}
	shim, err := pipeline.DevShim(c.options("", false, false), *dataDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "\nshim: %s\n  game/ -> %s\n\n", shim.Dir, shim.GameTarget)

	argv := []string{"--yes"}
	if !*open {
		argv = append(argv, "--no-open")
	}
	if *port != 0 {
		argv = append(argv, "--port", fmt.Sprint(*port))
	}
	if shim.DataDir != "" {
		argv = append(argv, "--data-dir", shim.DataDir)
	}

	cmd := exec.Command(shim.Launcher, argv...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Dir = shim.Dir
	// The launcher's sidecar state is machine-local and must not land in the
	// source tree; keep it beside the shim.
	cmd.Env = append(os.Environ(), "XDG_STATE_HOME="+filepath.Join(shim.Dir, "state"))
	return cmd.Run()
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	jsonLog := fs.Bool("json", false, "emit the event stream as JSON lines")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := fs.Arg(0)
	if dir == "" {
		return fmt.Errorf("usage: kobra-pack verify <dist-dir>")
	}
	logger := pack.NewLogger(os.Stderr, *jsonLog)
	return pack.VerifyDirectory(dir, logger)
}

func abs(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return absolute
}
