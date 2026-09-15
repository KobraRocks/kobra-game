package update

import (
	"context"

	"kobragames.local/launcher/internal/kobraerr"
)

// CheckResult is the outcome of an update check (§19.2). It is what the
// launcher reports to the shell as
// {"current":…,"latest":…,"available":…,"notes_url":…}.
type CheckResult struct {
	// Current is the installed release, from launcher.config.json's release
	// field.
	Current string
	// Latest is the newest published release, from latest.json.
	Latest string
	// Available is true when Latest is newer than Current.
	Available bool
	// NotesURL is the release-notes link declared by latest.json, if any.
	NotesURL string
	// Detail is a short, path-free explanation for diagnostics and for the
	// apply path. It never contains a path (§22.4).
	Detail string
}

// Check compares the installed release against the latest published one
// (§19.2). It fetches only <baseURL>/latest.json — never the patch, and never
// the manifest — so a check is cheap and stays within the user-initiated,
// fail-silently contract of FR-UPD-2. A network failure is returned as an
// error for the caller to swallow; it is not a CheckResult.
//
// installedRelease is the value from launcher.config.json's release field. The
// launcher_min gate of §19.5 needs a launcher version that Check does not
// take; the apply path MUST call MeetsLauncherMin on the fetched manifest
// before Verify (see that function's comment).
func Check(ctx context.Context, baseURL, installedRelease string) (CheckResult, error) {
	latest, err := FetchLatest(ctx, baseURL)
	if err != nil {
		logInfo("update.check", map[string]any{
			"current":   installedRelease,
			"latest":    "",
			"available": false,
		})
		return CheckResult{}, err
	}

	res := CheckResult{
		Current:  installedRelease,
		Latest:   latest.Release,
		NotesURL: latest.NotesURL,
	}
	res.Available = ReleaseNewer(latest.Release, installedRelease)
	if res.Available {
		res.Detail = "A newer release is available."
	} else {
		res.Detail = "The installed release is up to date."
	}

	logInfo("update.check", map[string]any{
		"current":   res.Current,
		"latest":    res.Latest,
		"available": res.Available,
	})
	return res, nil
}

// GuardApply is the §19.5 gate the apply path calls between FetchManifest and
// Verify. It refuses when the manifest demands a newer launcher, returning the
// E29 wording in the error's Detail so the caller can show it verbatim. Check
// cannot perform this gate itself — §19.2 gives it no launcher version and no
// manifest — which is why the gate lives here and MeetsLauncherMin is
// exported.
func GuardApply(m *Manifest, launcherVersion string) error {
	ok, msg := MeetsLauncherMin(m, launcherVersion)
	if ok {
		return nil
	}
	logWarn("update.verify.fail", map[string]any{"release": m.Release, "reason": "launcher_min"})
	return kobraerr.IO(msg, map[string]any{"reason": "launcher_min"}, nil)
}
