package update

// This file implements the configuration merge of Updater spec §14.
//
// # Why a merge is needed at all
//
// The update archive contains launcher/launcher.config.json: Packaging spec
// §9.3 requires launcher/ to be replaced in place, and the manifest index lists
// the file. But that file also holds machine-local, user-owned state — most
// importantly update.channel and update.patch_base_url, which is how the patch
// path is enabled in the first place.
//
// A naive swap therefore reverts local configuration. In the shipped fixture
// both releases declare "channel": "manual" while the installed config has been
// switched to "patch", so applying an update would silently disable the very
// feature that applied it and POST /api/update/apply would become unreachable.
//
// The rule is local-wins for machine-local keys and release-wins for
// release-owned keys (spec R14.2). The document is merged as raw JSON rather
// than through the typed config so that a key the launcher does not know about
// survives (R14.4) and so that absent keys are not materialised as zero values.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"kobragames.local/launcher/internal/config"
	"kobragames.local/launcher/internal/kobraerr"
	"kobragames.local/launcher/internal/paths"
)

// configRelPath is the one config file this merge owns, relative to a game
// folder or to the staging root. It is a variable so tests can point the merge
// at a fixture; production never changes it.
var configRelPath = paths.ConfigRelPath

// releaseOwnedKeys are the keys whose value comes from the release when both
// documents declare them (spec R14.2). "release" is the important one: the
// archive deliberately omits game/release.manifest.json (Packaging spec §9.1),
// so after a swap the installed release is reported from launcher.config.json's
// release field, and §15's retention logic depends on it (R14.3).
//
// The rest of the table is identity and presentation data that belongs to the
// release, not to the machine: letting a local copy win would leave an updated
// game still calling itself by the previous release's name, with the previous
// release's MIME types and locale list. A key only the local document declares
// is still preserved (R14.4); "release wins" applies to conflicts only.
var releaseOwnedKeys = map[string]bool{
	"release":    true,
	"game_id":    true,
	"game_name":  true,
	"mime_types": true,
	"locales":    true,
	"schema":     true,
}

// ConfigMergeOutcome reports what the merge did, for the event log. Only key
// names travel, never values (R14.8).
type ConfigMergeOutcome struct {
	// Merged is true when a merged document was written into the staging tree.
	Merged bool
	// FromLocal and FromRelease list the top-level key names taken from each
	// side. A key in neither list was absent from both.
	FromLocal   []string
	FromRelease []string
	// ReleasePresent records whether the release shipped a config at all.
	ReleasePresent bool
}

// MergeConfigInto replaces the release's launcher.config.json inside the
// staging tree with a merge of the release's copy and the installed local copy
// (spec R14.1). It is called after extraction and before the swap, and it
// returns the E28 wording on any failure: an update that would install a config
// the next launch refuses must not be installed at all (R14.5).
//
// A missing local copy is not an error: the release's copy is installed as-is
// (R14.7). A missing release copy is not an error either: nothing is merged,
// and the local copy survives because launcher/ is not swapped away (R14.6).
func MergeConfigInto(gameFolder, stagingRoot string) (ConfigMergeOutcome, error) {
	var out ConfigMergeOutcome

	// The staged launcher tree is launcher.new/, not launcher/: Extract strips
	// the archive's prefix (spec §9.2 step 10), so the release's copy of the
	// config lives under the staging name while the local copy keeps the
	// game-folder name.
	releasePath := filepath.Join(stagingRoot, launcherStagingDirName, filepath.Base(configRelPath))
	localPath := filepath.Join(gameFolder, filepath.FromSlash(configRelPath))

	releaseDoc, releaseOK, err := readConfigDoc(releasePath)
	if err != nil {
		return out, configDamaged("release_config_unreadable", err)
	}
	if !releaseOK {
		// R14.6: the archive did not ship a config. Nothing to merge.
		logInfo("update.config.merge", map[string]any{"result": "absent"})
		return out, nil
	}
	out.ReleasePresent = true

	localDoc, localOK, err := readConfigDoc(localPath)
	if err != nil {
		// R14.7: a local config we cannot read must not block the update.
		logWarn("update.config.merge", map[string]any{"result": "local_unreadable"})
		localOK = false
		localDoc = nil
	}

	merged, fromLocal, fromRelease := mergeConfigDocs(localDoc, releaseDoc)
	out.FromLocal = fromLocal
	out.FromRelease = fromRelease

	// R14.5: validate before the swap, never after.
	raw, err := json.Marshal(merged)
	if err != nil {
		return out, configDamaged("merge_encode", err)
	}
	if _, err := config.Parse(raw); err != nil {
		return out, configDamaged("merge_invalid", err)
	}

	// Write the *validated* document so defaults the schema supplies are
	// materialised and the installed file is canonical.
	cfg, err := config.Parse(raw)
	if err != nil {
		return out, configDamaged("merge_invalid", err)
	}
	final, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return out, configDamaged("merge_encode", err)
	}
	final = append(final, '\n')
	if err := writeFileAtomicMode(releasePath, final, 0o644); err != nil {
		return out, configDamaged("merge_write", err)
	}

	out.Merged = true
	if localOK {
		logInfo("update.config.merge", map[string]any{
			"result":       "merged",
			"from_local":   strings.Join(fromLocal, ","),
			"from_release": strings.Join(fromRelease, ","),
		})
	} else {
		logInfo("update.config.merge", map[string]any{"result": "release_only"})
	}
	return out, nil
}

// mergeConfigDocs merges two decoded config documents (spec R14.2, R14.4).
//
// The merge is recursive, because the conflicting values live inside nested
// objects: launcher.config.json's "update" object holds both the local
// patch_base_url (machine-local, local wins) and nothing release-owned, while
// "diagnostics" holds only local values. A shallow merge would let either
// side's whole object win and silently discard the other's keys — which is
// exactly the trap of §14.1, one level down.
//
// For every key:
//
//   - a release-owned key (R14.2 table: "release", "game_id", "game_name",
//     "mime_types", "locales", "schema") takes the release's value when the
//     release declares it;
//   - two objects are merged recursively;
//   - otherwise the local value wins when present;
//   - a key only the release declares is added;
//   - a key only the local document declares — including one this launcher does
//     not know about — is preserved as-is (R14.4).
func mergeConfigDocs(local, release map[string]any) (merged map[string]any, fromLocal, fromRelease []string) {
	merged = make(map[string]any, len(release)+len(local))
	keys := make([]string, 0, len(release)+len(local))
	seen := make(map[string]bool, len(release)+len(local))
	for k := range release {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for k := range local {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	for _, k := range keys {
		rv, inRelease := release[k]
		lv, inLocal := local[k]
		rm, rIsObj := rv.(map[string]any)
		lm, lIsObj := lv.(map[string]any)

		// A release-owned key takes the release's value even when both sides
		// are objects: the R14.2 owner table is about the key, not the leaf, so
		// a local mime_types or locales may not merge its way into the release's.
		if releaseOwnedKeys[k] && inRelease {
			merged[k] = rv
			fromRelease = append(fromRelease, k)
			continue
		}

		// Two objects: recurse, so each side keeps the keys it owns.
		if inRelease && inLocal && rIsObj && lIsObj {
			sub, subLocal, subRelease := mergeConfigDocs(lm, rm)
			merged[k] = sub
			if len(subLocal) > 0 || len(subRelease) > 0 {
				fromLocal = append(fromLocal, prefixKeys(k, subLocal)...)
				fromRelease = append(fromRelease, prefixKeys(k, subRelease)...)
			} else {
				fromLocal = append(fromLocal, k)
			}
			continue
		}

		switch {
		case inLocal:
			merged[k] = lv
			fromLocal = append(fromLocal, k)
		case inRelease:
			merged[k] = rv
			fromRelease = append(fromRelease, k)
		}
	}
	return merged, fromLocal, fromRelease
}

// prefixKeys renders nested key paths for the merge event, so the log names
// "update.channel" rather than a bare "update". Only key names travel, never
// values (R14.8).
func prefixKeys(prefix string, keys []string) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, prefix+"."+k)
	}
	return out
}

// readConfigDoc decodes a config file into a generic document. A missing file
// returns ok=false with a nil error.
func readConfigDoc(path string) (map[string]any, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, false, err
	}
	if doc == nil {
		return nil, false, errors.New("config document is not a JSON object")
	}
	return doc, true, nil
}

// writeFileAtomicMode is writeFileAtomic with a caller-chosen mode. The config
// file is a human-readable document rather than private state, so it keeps the
// conventional 0644 of the archive it replaces.
func writeFileAtomicMode(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(mode); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	syncDir(dir)
	return nil
}

// configDamaged is the E28 outcome for a merge that cannot produce a usable
// config (spec §16.2, R14.5).
func configDamaged(reason string, cause error) error {
	return kobraerr.IO(msgDamaged, map[string]any{"reason": reason}, fmt.Errorf("update: config merge: %w", cause))
}
