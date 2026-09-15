// Command kobra-launcher is the single native launcher binary (Launcher spec
// §4). It parses flags, runs the fixed startup sequence, serves the game over
// loopback, and drains on idle or signal.
//
// Exit codes (§2.5):
//
//	0  normal exit: idle shutdown, signal drain, or handoff to a live instance
//	1  startup failure with a user-facing message
//	3  update application failed but the game is still runnable
//	4  panic recovered by the top-level handler
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"kobragames.local/launcher/internal/browser"
	"kobragames.local/launcher/internal/config"
	"kobragames.local/launcher/internal/diagnostics"
	"kobragames.local/launcher/internal/paths"
	"kobragames.local/launcher/internal/port"
	"kobragames.local/launcher/internal/server"
	"kobragames.local/launcher/internal/sidecar"
	"kobragames.local/launcher/internal/storage"
	"kobragames.local/launcher/internal/update"
)

// Exit codes.
const (
	exitOK      = 0
	exitStartup = 1
	exitUpdate  = 3
	exitPanic   = 4
)

// Build metadata, injected with -ldflags (§25.2).
var (
	version   = "0.1.0-dev"
	commit    = "unknown"
	buildTime = "unknown"
)

// Engine and save format versions written into save headers.
const (
	engineVersion = "0.1.0"
	saveVersion   = 1
)

// --- flags ----------------------------------------------------------------

type flags struct {
	port        portValue
	checkPort   checkPort
	yes         bool
	dataDir     string
	browserPath string
	noOpen      bool
	printURL    bool
	diagnostics bool
	resetOrigin bool
	repair      bool
	logLevel    string
	maxLifetime time.Duration
	showVersion bool
}

// portValue is a uint16 flag for --port. A separate type from checkPort keeps
// "unset" distinguishable from "set to the default".
type portValue struct {
	set   bool
	value uint16
}

func (p *portValue) String() string { return "" }
func (p *portValue) Set(s string) error {
	v, err := parsePort(s)
	if err != nil {
		return err
	}
	p.set = true
	p.value = v
	return nil
}

// checkPort is an optional-value flag: --check-port uses the deterministic
// default, --check-port=N uses N.
type checkPort struct {
	set   bool
	value uint16
}

func (c *checkPort) String() string {
	if c == nil || !c.set {
		return ""
	}
	return fmt.Sprint(c.value)
}

func (c *checkPort) Set(s string) error {
	c.set = true
	if s == "" || s == "true" {
		return nil
	}
	n, err := parsePort(s)
	if err != nil {
		return err
	}
	c.value = n
	return nil
}

// IsBoolFlag lets the flag package accept a bare --check-port while Set still
// receives an explicit value when one is given.
func (c *checkPort) IsBoolFlag() bool { return true }

func parsePort(s string) (uint16, error) {
	// strconv.Atoi, not fmt.Sscanf: Sscanf stops at the first non-digit and
	// would accept "8080abc" as 8080.
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, errors.New("not a port number")
	}
	if n < 1 || n > 65535 {
		return 0, errors.New("port must be between 1 and 65535")
	}
	return uint16(n), nil
}

func main() { os.Exit(run()) }

// run holds the startup sequence of §4 so every failure path returns an exit
// code instead of calling os.Exit deep inside.
func run() int {
	fl := flags{}
	fs := flag.NewFlagSet("kobra-launcher", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Var(&fl.port, "port", "use this port without prompting (validated)")
	fs.Var(&fl.checkPort, "check-port", "report whether the port (or the default) is free, then exit")
	fs.BoolVar(&fl.yes, "yes", false, "accept the proposed port without prompting")
	fs.StringVar(&fl.dataDir, "data-dir", "", "place data/ outside the game folder")
	fs.StringVar(&fl.browserPath, "browser", "", "use a specific browser executable")
	fs.BoolVar(&fl.noOpen, "no-open", false, "start the server without opening a browser")
	fs.BoolVar(&fl.printURL, "print-url", false, "print the origin (with token) to stdout and block on the server")
	fs.BoolVar(&fl.diagnostics, "diagnostics", false, "print the diagnostics payload and exit")
	fs.BoolVar(&fl.resetOrigin, "reset-origin-state", false, "clear the remembered port")
	fs.BoolVar(&fl.repair, "repair", false, "roll back an interrupted update and rebuild data/ scaffolding")
	fs.StringVar(&fl.logLevel, "log-level", "info", "error|warn|info|debug")
	fs.DurationVar(&fl.maxLifetime, "max-lifetime", 0, "testing only: force a drain after this duration")
	fs.BoolVar(&fl.showVersion, "version", false, "print the launcher version and exit")
	// Development-only flags (--game-dir) are registered only in a kobra_dev
	// build; see devflag_dev.go / devflag_release.go.
	registerDevFlags(fs)

	// Step 1: parse flags.
	if err := fs.Parse(os.Args[1:]); err != nil {
		return exitStartup
	}
	if fl.showVersion {
		fmt.Printf("kobra-launcher %s (%s) built %s\n", version, commit, buildTime)
		return exitOK
	}
	if hasDevGameDir() {
		devNotice()
	}

	// Fast-path flag contract of §4.1.
	if fl.printURL && (fl.checkPort.set || fl.diagnostics) {
		fmt.Fprintln(os.Stderr, "--print-url is incompatible with --check-port and --diagnostics")
		return exitStartup
	}
	if fl.printURL {
		fl.noOpen = true
	}
	level, err := diagnostics.ParseLevel(fl.logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitStartup
	}

	// Step 2: resolve the executable path.
	exe, err := os.Executable()
	if err != nil {
		fail(paths.UserMessage(paths.ErrExecutable))
		return exitStartup
	}

	// Steps 3a/3b: finish an interrupted swap, then install any update the user
	// requested, all BEFORE the strict layout check.
	//
	// §19.4 requires recovery at startup, and its own scenario can leave game/
	// missing — which is exactly the state paths.Resolve rejects. Running
	// recovery first is therefore the only order that works: the launcher must
	// be able to restore a runnable release before it can insist on finding one.
	//
	// The update apply of Updater spec §7 then runs in the same window, before
	// the instance lock, the port and any socket exist (spec R7.3), so no client
	// can observe a half-swapped game folder. Both passes need only the game
	// folder, which is located from the executable path exactly as §5.1 does.
	//
	// --check-port and --diagnostics answer "can I run?" and must not mutate the
	// install to do it (spec R7.4), so they run recovery only.
	var startupRes update.StartupResult
	if folder, ferr := devGameFolderForRecovery(exe); ferr == nil {
		applyPass := !fl.checkPort.set && !fl.diagnostics
		var sidecarDir string
		if applyPass {
			// The sidecar is needed before config validation, so the game id is
			// read best-effort. An unreadable config disables the apply and the
			// launcher fails below with the real config error.
			gameID := ""
			if early, cerr := config.Load(update.ConfigPathForFolder(folder)); cerr == nil {
				gameID = early.GameID
			}
			sidecarDir = update.SidecarDirForGame(folder, gameID)
		}
		startupRes = update.Startup(context.Background(), update.StartupOptions{
			GameFolder:      folder,
			SidecarDir:      sidecarDir,
			LauncherVersion: version,
			Apply:           update.ApplyOptions{LauncherVersion: version},
		})
	}

	// Step 3c: resolve the game folder (FR-LNCH-1, or the dev override).
	root, err := devResolve(exe, fl.dataDir)
	if err != nil {
		fail(paths.UserMessage(err))
		return exitStartup
	}

	// Step 4: load and validate launcher.config.json (FR-SCH-3).
	cfg, err := config.Load(root.ConfigPath)
	if err != nil {
		fail("The launcher's configuration is invalid: " + firstLine(err.Error()))
		return exitStartup
	}
	// server.probe_path is documented as configurable but the endpoint is part
	// of the protocol the shell speaks. Refuse a value the launcher does not
	// serve rather than silently ignoring it.
	if err := cfg.ValidateProbePath(port.ProbePath); err != nil {
		fail("The launcher's configuration is invalid: " + firstLine(err.Error()))
		return exitStartup
	}

	// Step 5: initialise diagnostics. A sidecar failure degrades rather than
	// failing startup (§6.3).
	root, log, sidecarErr := initRootAndDiagnostics(root, cfg, level, fl.maxLifetime != 0)
	defer log.Close()
	logStartup(log, cfg, root, sidecarErr)
	browser.SetLogger(log)
	update.SetLogger(log)

	// Report the earlier recovery outcome now that there is a log to write to.
	if startupRes.Recovered != update.RecoveryNone {
		log.Warn("update.swap.rollback", map[string]any{
			"release": startupRes.Applied.TargetRelease,
			"reason":  string(startupRes.Recovered),
		})
	}
	// Report what the §7 startup update pass did. A failed apply never stops
	// startup (Updater spec R7.9); it is recorded so a user can be asked for a
	// log tail (R7.10).
	switch startupRes.Applied.Outcome {
	case update.ApplyApplied:
		log.Info("update.apply.done", map[string]any{
			"target":    startupRes.Applied.TargetRelease,
			"installed": startupRes.Applied.InstalledRelease,
		})
	case update.ApplyFailed, update.ApplyCancelled:
		log.Warn("update.apply.failed", map[string]any{
			"reason":    startupRes.Applied.Reason,
			"retryable": startupRes.Applied.Retryable,
		})
	}

	// Step 6: fast paths return here.
	denyList := loadDenyList(cfg, root, log)
	if fl.checkPort.set {
		return runCheckPort(fl.checkPort, cfg, denyList, log)
	}
	if fl.diagnostics {
		return runDiagnostics(root, cfg, log)
	}
	if fl.repair {
		if err := runRepair(root, log); err != nil {
			fail("The repair could not be completed.")
			return exitUpdate
		}
	}
	if fl.resetOrigin {
		_ = os.Remove(sidecar.HeldLockPath(root.SidecarDir))
		_ = os.Remove(root.SidecarDir + string(os.PathSeparator) + sidecar.PortFileName)
		log.Info("port.reset", nil)
	}

	// Step 7: acquire or detect the instance lock (FR-SRV-25).
	lock, heldInfo, err := acquireInstance(root, cfg, log)
	if err != nil {
		if heldInfo != nil {
			// §9.4 handoff: open the existing origin and exit 0.
			log.Info("instance.handoff", map[string]any{"existing_port": heldInfo.Port})
			if !fl.noOpen {
				openBrowser(fl, cfg, sidecar.Origin(heldInfo.Port), "", log)
			}
			return exitOK
		}
		fail("Another launcher is starting. Try again in a moment.")
		return exitStartup
	}
	defer func() {
		if lock != nil {
			_ = lock.Release()
		}
	}()

	// Step 8: allocate the port.
	state, _ := sidecar.Load(root.SidecarDir)
	candidate, src := selectCandidate(state, cfg)
	port.LogCandidate(log, candidate, src)

	chosen, ln, err := allocatePort(fl, candidate, cfg, denyList, log)
	if err != nil {
		fail(portFailureMessage(err))
		return exitStartup
	}
	if chosen != candidate {
		port.LogChange(log, candidate, chosen)
	}

	// Step 9: probe data/ write access (FR-SAVE-16).
	eng, err := storage.New(storage.Options{
		DataDir:            root.DataDir,
		DataDirKind:        root.DataDirKind,
		KeepRevisions:      cfg.DataAPI.KeepRevisions,
		TrashRetentionDays: cfg.DataAPI.TrashRetentionDays,
		MaxRequestBytes:    cfg.DataAPI.MaxRequestBytes,
		GameID:             cfg.GameID,
		Release:            cfg.Release,
		EngineVersion:      engineVersion,
		SaveVersion:        saveVersion,
		Logger:             log,
	})
	if err != nil {
		fail("The launcher could not open the game's data folder.")
		return exitStartup
	}
	if err := eng.ProbeWrite(); err != nil {
		log.Warn("storage.write.probe.failed", map[string]any{"reason": eng.DataWritableReason()})
	}
	_ = eng.InitRevisions(context.Background())
	_, _ = eng.CleanupTrash(context.Background())

	// Steps 10-11: build the server on the bound socket, complete the lock and
	// write port.json.
	srv := server.New(server.Options{
		Root:    root,
		Cfg:     cfg,
		Storage: eng,
		Log:     log,
		Port:    chosen,
		Version: version,
		Commit:  commit,
		Release: cfg.Release,
		// Rollback retention (Updater spec §15). The count advances only once
		// the shell has actually connected, because "started successfully"
		// means the launcher reached SERVE *and* a heartbeat arrived.
		OnFirstHeartbeat: func() {
			// The release is resolved the same way the rest of the launcher
			// resolves it (§14.3), not read from the config: the counter is
			// keyed by release, and a key that disagreed with the folder's
			// actual release would reset the count to 1 on every start —
			// leaving game.old in place forever, which is the bug this hook
			// exists to fix.
			installed := update.InstalledReleaseIn(root.GameFolder)
			if installed == "" {
				installed = cfg.Release
			}
			update.NoteSuccessfulStart(root.GameFolder, root.SidecarDir, installed)
		},
		// §20.5: the page opened but never reached the launcher. The spec
		// requires this to be surfaced with the log location, so this message —
		// unlike an API error envelope — names it.
		OnWatchdogTimeout: func(logDir string) {
			if logDir == "" {
				fail("The game opened but couldn't reach the launcher.")
				return
			}
			fail("The game opened but couldn't reach the launcher.\nSee " +
				logDir + string(os.PathSeparator) + "launcher.log" +
				", or run the launcher with --diagnostics to copy them.")
		},
	})
	srv.Bind(ln)
	if err := lock.SetPort(chosen, srv.Instance()); err != nil {
		log.Warn("instance.lock.update.failed", nil)
	}
	if err := persistPortState(root, cfg, chosen); err != nil {
		log.Warn("sidecar.port.write.failed", map[string]any{"reason": "write"})
	}

	// Steps 12-14: detect a compatible browser, issue the bootstrap token, open
	// the page. The token is created before the browser is opened so the
	// fragment always carries one (§4.2: token after server start).
	if fl.printURL || !fl.noOpen {
		path, name, bErr := detectBrowser(fl, cfg)
		if path == "" {
			if !fl.printURL {
				// §20.4: guidance, then exit 1. The launcher never launches an
				// unsupported browser "just to see".
				log.Warn("browser.none", map[string]any{"reason": browserReason(bErr)})
				fail(browser.InstallGuidance())
				return exitStartup
			}
			log.Warn("browser.none", map[string]any{"reason": browserReason(bErr)})
		} else {
			srv.SetBrowser(path, name)
		}
	}
	token, err := srv.Sessions().NewBootstrapToken()
	if err != nil {
		fail("The launcher could not create a session token.")
		return exitStartup
	}
	if fl.printURL {
		fmt.Printf("%s/index.html#t=%s\n", srv.Origin(), token)
		_ = os.Stdout.Sync()
	} else if !fl.noOpen {
		openBrowser(fl, cfg, srv.Origin(), token, log)
	}

	// A testing-only lifetime bound (§2.3).
	if fl.maxLifetime > 0 {
		go func() {
			time.Sleep(fl.maxLifetime)
			log.Info("server.stop", map[string]any{"reason": "max_lifetime"})
			srv.Shutdown("max_lifetime")
		}()
	}

	// Step 15: signals start the drain. A second signal during drain is logged
	// and ignored (§2.4).
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		first := true
		for sig := range sigCh {
			if !first {
				log.Warn("server.signal.ignored", map[string]any{"signal": sig.String()})
				continue
			}
			first = false
			if sig == syscall.SIGQUIT {
				log.Warn("server.signal.quit", nil)
			}
			log.Info("server.stop", map[string]any{"reason": "signal"})
			srv.Shutdown("signal")
		}
	}()

	// Steps 16-18: block until drain, then exit. A drain caused by a recovered
	// handler panic is reported as the documented exit code 4 (FR-SRV-24)
	// rather than a normal exit: it is the only signal an operator gets that a
	// crash dump was written.
	if err := srv.Serve(); err != nil {
		log.Error("server.serve.failed", map[string]any{"reason": "listener"})
		return exitStartup
	}
	if srv.Panicked() {
		log.Error("launcher.exit", map[string]any{"code": exitPanic})
		return exitPanic
	}
	log.Info("launcher.exit", map[string]any{"code": exitOK})
	return exitOK
}

// --- startup helpers ------------------------------------------------------

// initRootAndDiagnostics resolves the sidecar directory and opens the log,
// recording a fallback rather than failing (§6.3).
func initRootAndDiagnostics(root paths.Root, cfg config.Config, level diagnostics.Level, mirrorStderr bool) (paths.Root, *diagnostics.Logger, error) {
	native, nerr := paths.SidecarDirFor(cfg.GameID)
	if nerr != nil {
		root.SetSidecarFallbackReason("The machine-local state folder could not be located.")
	}
	if err := root.SetSidecar(native, paths.FallbackSidecarDir(root.GameFolder)); err != nil {
		// Neither location is writable: the launcher still runs, with a logger
		// that mirrors to stderr.
		root.SidecarDir = paths.FallbackSidecarDir(root.GameFolder)
		root.LogDir = root.SidecarDir + string(os.PathSeparator) + "logs"
		root.SidecarKind = "fallback"
		log, _ := diagnostics.Open(diagnostics.Options{
			LogDir:       "",
			Level:        level,
			MirrorStderr: true,
		})
		return root, log, errors.New("sidecar and fallback both unwritable")
	}
	log, openErr := diagnostics.Open(diagnostics.Options{
		LogDir:       root.LogDir,
		MaxBytes:     cfg.Diagnostics.LogMaxBytes,
		Generations:  cfg.Diagnostics.LogRetentionFiles,
		Level:        level,
		Version:      version,
		Commit:       commit,
		MirrorStderr: mirrorStderr,
	})
	log.WatchSIGHUP()
	if openErr != nil {
		return root, log, openErr
	}
	return root, log, nil
}

func logStartup(log *diagnostics.Logger, cfg config.Config, root paths.Root, sidecarErr error) {
	log.Info("launcher.start", map[string]any{
		"version": version,
		"commit":  commit,
		"os":      runtime.GOOS,
		"arch":    runtime.GOARCH,
	})
	log.Info("launcher.folder.resolved", map[string]any{"game_folder_kind": root.FolderKind})
	if root.FolderKind == "override" {
		log.Warn("launcher.folder.override", map[string]any{"reason": "development_build"})
	}
	log.Info("config.loaded", map[string]any{"game_id": cfg.GameID, "release": cfg.Release})
	log.Info("sidecar.dir", map[string]any{"kind": root.SidecarKind})
	if sidecarErr != nil {
		log.Warn("sidecar.fallback", map[string]any{"reason": "unavailable"})
	}
}

// firstLine trims a config or schema failure to its first line, because a
// user-facing message must stay short (it is printed to stderr, and a caller may
// put it in a dialog).
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func fail(msg string) { fmt.Fprintln(os.Stderr, msg) }

// --- port allocation ------------------------------------------------------

func loadDenyList(cfg config.Config, root paths.Root, log *diagnostics.Logger) *port.DenyList {
	p := cfg.DenyListPath(root.GameFolder)
	if p == "" {
		return nil
	}
	d, err := port.LoadDenyList(p)
	if err != nil {
		log.Warn("port.deny.load.failed", map[string]any{"reason": "unavailable"})
		return nil
	}
	return d
}

func selectCandidate(state *sidecar.State, cfg config.Config) (uint16, port.Source) {
	if state != nil && state.Port >= 1024 && state.Port <= 65535 {
		return state.Port, port.SourceSaved
	}
	return port.Default(cfg.GameID, cfg.Port.Base, cfg.Port.Span), port.SourceDefault
}

// allocatePort validates, optionally confirms, and binds (§8.3-§8.5).
func allocatePort(fl flags, candidate uint16, cfg config.Config, deny *port.DenyList, log *diagnostics.Logger) (uint16, net.Listener, error) {
	// --port is explicit intent: it is validated and never silently
	// substituted (§8.7).
	if fl.port.set {
		if err := port.Validate(fl.port.value, deny); err != nil {
			return 0, nil, err
		}
		chosen, ln, err := port.AvailableLogged(log, fl.port.value, deny, nil, port.DefaultProbeTimeout, cfg.GameID)
		if err != nil {
			return 0, nil, err
		}
		port.LogConfirmed(log, chosen, false)
		return chosen, ln, nil
	}
	if err := port.Validate(candidate, deny); err != nil {
		var de *port.DenyError
		if !errors.As(err, &de) {
			return 0, nil, err
		}
		// A saved or computed candidate the deny list forbids is substituted
		// by the scan rather than treated as fatal.
		exclude := map[uint16]bool{candidate: true}
		p, serr := port.ScanLogged(log, cfg.Port.Base, cfg.Port.Span, deny, exclude, port.DefaultProbeTimeout, cfg.GameID)
		if serr != nil {
			return 0, nil, serr
		}
		candidate = p
	}
	if cfg.Port.RequireConfirmation && !fl.yes && isInteractive() {
		confirmed, err := confirmPort(candidate, deny, log)
		if err != nil {
			return 0, nil, err
		}
		candidate = confirmed
	}
	chosen, ln, err := port.AvailableLogged(log, candidate, deny, nil, port.DefaultProbeTimeout, cfg.GameID)
	if err != nil {
		return 0, nil, err
	}
	port.LogConfirmed(log, chosen, false)
	return chosen, ln, nil
}

func portFailureMessage(err error) string {
	if errors.Is(err, port.ErrNoPort) {
		return "No usable port could be found. Close other applications or choose a port manually with --port."
	}
	return "The launcher could not start: " + err.Error()
}

// --- port confirmation prompt (§8.5) --------------------------------------

// confirmPort asks the user to accept or change the proposed port. It times out
// to the proposed default after 60 seconds so an unattended launcher does not
// hang (FR-A11Y-5). The native-dialog variant of §8.5 is a Windows/macOS
// concern; on Linux this is the terminal prompt the spec names.
func confirmPort(proposed uint16, deny *port.DenyList, log *diagnostics.Logger) (uint16, error) {
	deadline := time.Now().Add(60 * time.Second)
	fmt.Printf("\nThe game will be served at:\n\n    %s\n\n[ Enter = OK, or type a port ] ", sidecar.Origin(proposed))
	for {
		line, ok := readLine(time.Until(deadline))
		if !ok {
			fmt.Println("\nNo answer after 60 seconds; continuing with the proposed port.")
			return proposed, nil
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return proposed, nil
		}
		p, err := parsePort(line)
		if err != nil {
			// Invalid input re-prompts instead of being silently discarded:
			// keeping the proposal is a decision the user did not make.
			fmt.Printf("%s; try again, or press Enter for %s. ", err.Error(), sidecar.Origin(proposed))
			continue
		}
		if err := port.Validate(p, deny); err != nil {
			// The user (or a shell using its own copy of the deny list) accepted
			// a port this launcher forbids: record the disagreement (§6.4)
			// before refusing it.
			port.CheckDenyMismatch(log, p, true, deny)
			fmt.Printf("%s; try again, or press Enter for %s. ", err.Error(), sidecar.Origin(proposed))
			continue
		}
		return p, nil
	}
}

// readLine reads one line from stdin, giving up after d. The read runs on its
// own goroutine because a terminal read cannot be interrupted; when the deadline
// wins, that goroutine stays blocked until stdin closes, which is harmless for a
// process that is about to decide the port and exit or serve.
func readLine(d time.Duration) (string, bool) {
	type result struct {
		line string
		ok   bool
	}
	ch := make(chan result, 1)
	go func() {
		var line string
		if _, err := fmt.Scanln(&line); err != nil {
			ch <- result{ok: false}
			return
		}
		ch <- result{line: line, ok: true}
	}()
	select {
	case r := <-ch:
		return r.line, r.ok
	case <-time.After(d):
		return "", false
	}
}

// isInteractive reports whether stdin is a terminal.
func isInteractive() bool {
	st, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// --- fast paths -----------------------------------------------------------

func runCheckPort(cp checkPort, cfg config.Config, deny *port.DenyList, log *diagnostics.Logger) int {
	candidate := cp.value
	if candidate == 0 {
		candidate = port.Default(cfg.GameID, cfg.Port.Base, cfg.Port.Span)
	}
	result := "other"
	if err := port.Validate(candidate, deny); err != nil {
		result = "denied"
	} else if pr, err := port.ProbeLogged(log, candidate, port.DefaultProbeTimeout, cfg.GameID); err == nil {
		switch pr {
		case port.ProbeFree:
			result = "free"
		case port.ProbeOurs:
			result = "ours"
		case port.ProbeOtherGame:
			result = "other_game"
		}
	}
	out, _ := json.Marshal(map[string]any{
		"port":   candidate,
		"result": result,
		"free":   result == "free",
	})
	fmt.Println(string(out))
	return exitOK
}

func runDiagnostics(root paths.Root, cfg config.Config, log *diagnostics.Logger) int {
	state, _ := sidecar.Load(root.SidecarDir)
	history := []string{}
	if state != nil {
		history = state.OriginHistoryStrings()
	}
	payload := map[string]any{
		"launcher_version": version,
		"release":          cfg.Release,
		"engine_version":   engineVersion,
		"game_id":          cfg.GameID,
		"port":             candidatePort(state, cfg),
		"origin_history":   history,
		"data_dir_kind":    root.DataDirKind,
		"sidecar_kind":     root.SidecarKind,
		"log_tail":         log.Tail(200),
	}
	out, _ := json.MarshalIndent(payload, "", "  ")
	fmt.Println(string(out))
	return exitOK
}

func candidatePort(state *sidecar.State, cfg config.Config) uint16 {
	if state != nil && state.Port != 0 {
		return state.Port
	}
	return port.Default(cfg.GameID, cfg.Port.Base, cfg.Port.Span)
}

// runRepair is --repair: roll back an interrupted update and rebuild the data
// scaffolding. It reports exit code 3 (§2.5) when the release could not be left
// in a runnable state.
//
// Recovery itself already ran in the startup pass, before the layout check; by
// the time this function runs there is normally nothing left to recover. What
// it adds is the intent cleanup and the retention reset of Updater spec R15.9
// and R15.10: a rolled-back install has no backup to retire, and a stale count
// would retire the next update's backup prematurely.
func runRepair(root paths.Root, log *diagnostics.Logger) error {
	action, err := update.Recover(root.GameFolder, update.StagingRootFor(root.SidecarDir))
	if err != nil {
		return err
	}
	if action != update.RecoveryNone {
		log.Warn("update.swap.rollback", map[string]any{"release": "", "reason": string(action)})
	}
	update.ClearStarted(root.SidecarDir)
	update.ClearUpdateIntent(root.SidecarDir)
	if err := os.MkdirAll(root.DataDir, 0o755); err != nil {
		return errors.New("the data folder could not be created")
	}
	log.Info("repair.complete", map[string]any{"action": string(action)})
	return nil
}

// --- instance lock --------------------------------------------------------

func acquireInstance(root paths.Root, cfg config.Config, log *diagnostics.Logger) (*sidecar.Lock, *sidecar.LockInfo, error) {
	lock, err := sidecar.AcquireLock(root.SidecarDir, cfg.GameID, version, 0, "")
	if err != nil {
		var held *sidecar.LockHeldError
		if errors.As(err, &held) {
			// §9.4/§9.5: a live instance of the same game is reported to the
			// caller, which hands off.
			return nil, held.Info, err
		}
		return nil, nil, err
	}
	log.Info("instance.lock.acquired", map[string]any{"pid": os.Getpid()})
	if lock.Reclaimed() {
		log.Warn("instance.lock.reclaimed", map[string]any{"reason": "stale"})
	}
	return lock, nil, nil
}

// persistPortState writes port.json and appends the origin history (§8.6).
func persistPortState(root paths.Root, cfg config.Config, chosen uint16) error {
	state, _ := sidecar.Load(root.SidecarDir)
	if state == nil {
		state = &sidecar.State{GameID: cfg.GameID}
	}
	state.GameID = cfg.GameID
	state.Port = chosen
	state.Origin = sidecar.Origin(chosen)
	state.RememberOrigin(state.Origin)
	return state.Save(root.SidecarDir)
}

// --- browser --------------------------------------------------------------

// detectBrowser runs detection once for the configured preference order.
func detectBrowser(fl flags, cfg config.Config) (path, name string, err error) {
	if fl.browserPath != "" {
		return fl.browserPath, "custom", nil
	}
	cands, derr := browser.Detect(cfg.BrowserPreference, cfg.MinBrowserVersion)
	if derr != nil {
		// ErrNoBrowser and ErrTooOld are different situations and used to be
		// collapsed into ("",""): a browser too old for the API is not the same
		// failure as none installed, and the log has to say which happened.
		return "", "", derr
	}
	if len(cands) == 0 {
		return "", "", browser.ErrNoBrowser
	}
	return cands[0].Path, cands[0].Name, nil
}

// browserReason names why detection failed, without a path or an error string.
func browserReason(err error) string {
	switch {
	case errors.Is(err, browser.ErrTooOld):
		return "too_old"
	case errors.Is(err, browser.ErrNoBrowser):
		return "no_browser"
	default:
		return "unavailable"
	}
}

// openBrowser launches the browser at the origin. An empty token means the
// instance already owns the origin (the handoff case), where the existing
// session model applies.
func openBrowser(fl flags, cfg config.Config, origin, token string, log *diagnostics.Logger) {
	path, name, bErr := detectBrowser(fl, cfg)
	if path == "" {
		log.Warn("browser.none", map[string]any{"reason": browserReason(bErr)})
		return
	}
	url := origin + "/index.html"
	if token != "" {
		url += "#t=" + token
	}
	if err := browser.Launch(path, url); err != nil {
		log.Warn("browser.launch.failed", map[string]any{"name": name})
		return
	}
	log.Info("browser.launch", map[string]any{"name": name})
}
