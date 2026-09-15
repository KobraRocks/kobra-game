package storage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"kobragames.local/launcher/internal/diagnostics"
	"kobragames.local/launcher/internal/kobraerr"
)

// modManifest mirrors mod.manifest.schema.json's operational fields. Unknown
// fields are preserved in the file but not modelled here.
type modManifest struct {
	Schema   string   `json:"schema"`
	ID       string   `json:"id"`
	Name     string   `json:"name,omitempty"`
	Version  string   `json:"version,omitempty"`
	Priority int      `json:"priority,omitempty"`
	Requires []string `json:"requires,omitempty"`
	Enabled  *bool    `json:"enabled,omitempty"`
}

// ModInfo is one entry of the mods view (data-api.schema.json #/$defs/mods).
type ModInfo struct {
	ID                string `json:"id"`
	Name              string `json:"name,omitempty"`
	Version           string `json:"version,omitempty"`
	Enabled           bool   `json:"enabled"`
	Priority          int    `json:"priority,omitempty"`
	MissingDependency string `json:"missing_dependency,omitempty"`
}

// ModsView is the GET /api/data/config/mods response.
type ModsView struct {
	Enabled   []string  `json:"enabled"`
	Available []ModInfo `json:"available"`
}

// ModAction is a POST/DELETE mod request.
type ModAction struct {
	ID           string
	Action       string // install | enable | disable
	ArchiveBytes []byte // base64-decoded payload, when action is install
}

// modsState is the persisted enable/disable state in data/config/mods.json.
type modsState struct {
	Enabled []string `json:"enabled"`
}

// ReadMods enumerates data/mods/<id>/mod.manifest.json and merges the persisted
// enabled set. A mod whose declared dependency is absent is reported disabled
// with missing_dependency set rather than silently enabled (FS §10.4).
func (e *Engine) ReadMods() (ModsView, error) {
	view := ModsView{Enabled: []string{}, Available: []ModInfo{}}
	state := e.readModsState()
	enabledSet := map[string]bool{}
	for _, id := range state.Enabled {
		enabledSet[id] = true
	}

	dir := e.dirFor("mods")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return view, nil
		}
		return view, kobraerr.IO("The launcher could not read the mods folder.", nil, err)
	}
	installed := map[string]ModInfo{}
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		id := ent.Name()
		if err := ValidateIdentifier(id); err != nil {
			continue
		}
		info := ModInfo{ID: id}
		raw, err := os.ReadFile(filepath.Join(dir, id, "mod.manifest.json"))
		if err == nil {
			var m modManifest
			if json.Unmarshal(raw, &m) == nil {
				if m.Name != "" {
					info.Name = m.Name
				}
				info.Version = m.Version
				info.Priority = m.Priority
				for _, req := range m.Requires {
					if _, ok := installed[req]; !ok {
						if _, statErr := os.Stat(filepath.Join(dir, req)); statErr != nil {
							info.MissingDependency = req
						}
					}
				}
			}
		}
		info.Enabled = enabledSet[id] && info.MissingDependency == ""
		installed[id] = info
	}
	ids := make([]string, 0, len(installed))
	for id := range installed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		info := installed[id]
		view.Available = append(view.Available, info)
		if info.Enabled {
			view.Enabled = append(view.Enabled, id)
		}
	}
	sort.Strings(view.Enabled)
	return view, nil
}

// ModAction applies enable/disable/install.
func (e *Engine) ModAction(a ModAction) error {
	if err := ValidateIdentifier(a.ID); err != nil {
		return err
	}
	if !e.DataWritable() {
		return kobraerr.ReadOnly(e.DataWritableReason())
	}
	e.writeMu.Lock()
	defer e.writeMu.Unlock()

	state := e.readModsState()
	enabled := map[string]bool{}
	for _, id := range state.Enabled {
		enabled[id] = true
	}

	switch a.Action {
	case "enable":
		modDir := filepath.Join(e.dirFor("mods"), a.ID)
		if _, err := os.Stat(modDir); err != nil {
			return kobraerr.NotFound("mod")
		}
		enabled[a.ID] = true
	case "disable":
		delete(enabled, a.ID)
	case "install":
		if err := e.installModLocked(a); err != nil {
			return err
		}
		enabled[a.ID] = true
	default:
		return kobraerr.MalformedField("action", "That mod action is not supported.", nil)
	}

	ids := make([]string, 0, len(enabled))
	for id := range enabled {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	body, err := json.MarshalIndent(modsState{Enabled: ids}, "", "  ")
	if err != nil {
		return kobraerr.IO("The launcher could not save the mod list.", nil, err)
	}
	body = append(body, '\n')
	if err := e.writeAtomic(context.TODO(), e.modsPath(), body, 0o644); err != nil {
		return mapIOError(err)
	}
	e.logf(diagnostics.LevelInfo, "mod.action", map[string]any{"id": a.ID, "action": a.Action})
	return nil
}

// installModLocked unpacks a user-supplied .tar.zst mod archive into
// data/mods/<id>/. The archive format is the one §19 uses for updates; the mod
// archive layout question is deferred (§27.5), so this implementation accepts
// the same tar-based layout and rejects any entry that is not a regular file
// under the mod's own directory.
func (e *Engine) installModLocked(a ModAction) error {
	if len(a.ArchiveBytes) == 0 {
		return kobraerr.MalformedField("archive_bytes", "The mod archive was empty.", nil)
	}
	dest := filepath.Join(e.dirFor("mods"), a.ID)
	if err := extractTarZstd(a.ArchiveBytes, dest); err != nil {
		return kobraerr.MalformedField("archive_bytes", "That mod archive could not be unpacked.", err)
	}
	return nil
}

// readModsState reads config/mods.json, tolerating absence and corruption.
func (e *Engine) readModsState() modsState {
	var st modsState
	raw, err := os.ReadFile(e.modsPath())
	if err != nil {
		return st
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return modsState{}
	}
	clean := st.Enabled[:0]
	for _, id := range st.Enabled {
		if ValidateIdentifier(id) == nil {
			clean = append(clean, id)
		}
	}
	st.Enabled = clean
	return st
}

// EnabledModIDs returns the enabled set without the manifest scan.
func (e *Engine) EnabledModIDs() []string {
	st := e.readModsState()
	out := append([]string(nil), st.Enabled...)
	sort.Strings(out)
	return out
}

// modAssetPath resolves <mods>/<id>/assets/<rest>, used by static serving for
// /mods/<id>/assets/* (§13.1).
func (e *Engine) ModAssetPath(id, rest string) (string, error) {
	if err := ValidateIdentifier(id); err != nil {
		return "", err
	}
	clean := filepath.Clean("/" + strings.TrimPrefix(rest, "/"))
	full := filepath.Join(e.dirFor("mods"), id, "assets", filepath.FromSlash(clean))
	if err := e.confine(full); err != nil {
		return "", err
	}
	// The mod id itself must stay inside the mods directory too.
	if err := e.confine(filepath.Join(e.dirFor("mods"), id)); err != nil {
		return "", err
	}
	return full, nil
}

// DeleteMod moves a mod's directory into the trash, honouring the §14.7 rule
// that deletion never unlinks.
func (e *Engine) DeleteMod(id string) error {
	if err := ValidateIdentifier(id); err != nil {
		return err
	}
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	dir := filepath.Join(e.dirFor("mods"), id)
	if err := e.confine(dir); err != nil {
		return err
	}
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return kobraerr.NotFound("mod")
		}
		return mapIOError(err)
	}
	trash := e.newTrashDir("mods")
	if err := os.MkdirAll(trash, 0o755); err != nil {
		return mapIOError(err)
	}
	if err := os.Rename(dir, filepath.Join(trash, id)); err != nil {
		if !isEXDEV(err) {
			return mapIOError(err)
		}
		if err := os.RemoveAll(dir); err != nil {
			return mapIOError(err)
		}
	}
	// Keep the persisted enable list consistent with what is installed.
	state := e.readModsState()
	kept := state.Enabled[:0]
	for _, eid := range state.Enabled {
		if eid != id {
			kept = append(kept, eid)
		}
	}
	state.Enabled = kept
	if body, err := json.MarshalIndent(state, "", "  "); err == nil {
		_ = e.writeAtomic(context.TODO(), e.modsPath(), append(body, '\n'), 0o644)
	}
	e.logf(diagnostics.LevelInfo, "mod.action", map[string]any{"id": id, "action": "delete"})
	return nil
}
