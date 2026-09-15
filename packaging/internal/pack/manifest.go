package pack

import (
	"os"
	"path/filepath"
)

// Document shapes. They are structs rather than maps so that field order in the
// written JSON is fixed, which is part of what makes two builds byte-identical
// (§5.2).

// AssetManifest mirrors asset.manifest.json (Packaging spec §6.2).
type AssetManifest struct {
	Schema           string  `json:"schema"`
	Release          string  `json:"release"`
	Generated        string  `json:"generated"`
	ContentAddressed bool    `json:"content_addressed"`
	Assets           []Asset `json:"assets"`
}

// Asset is one advisory-hash entry. The hashes here do not gate anything at
// runtime (FR-AST-4); they exist so the launcher and a human can see what a
// release contains.
type Asset struct {
	Path    string `json:"path"`
	Type    string `json:"type"`
	Size    int64  `json:"size"`
	Hash    string `json:"hash"`
	Mutable bool   `json:"mutable"`
	Cache   string `json:"cache"`
}

// ReleaseManifest mirrors release.manifest.json (Packaging spec §6.1). It is the
// release's authority document and the thing a publisher signs.
type ReleaseManifest struct {
	Schema          string      `json:"schema"`
	Release         string      `json:"release"`
	PreviousRelease *string     `json:"previous_release"`
	Published       string      `json:"published"`
	LauncherMin     string      `json:"launcher_min"`
	EngineVersion   string      `json:"engine_version"`
	SaveVersion     int         `json:"save_version"`
	GameVersion     string      `json:"game_version"`
	Channel         string      `json:"channel"`
	Scope           string      `json:"scope"`
	TotalSize       int64       `json:"total_size"`
	Archive         *ArchiveRef `json:"archive,omitempty"`
	Files           []FileEntry `json:"files"`
	Migrations      []any       `json:"migrations"`
	Notes           string      `json:"notes,omitempty"`
	ReleaseNotesURL string      `json:"release_notes_url,omitempty"`
}

// ArchiveRef names the update archive this manifest describes (§8.2.2). The
// manifest is never a member of that archive: a manifest inside the archive it
// hashes would have to contain the input to its own hash.
type ArchiveRef struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Size   int64  `json:"size"`
	Hash   string `json:"hash"`
	Format string `json:"format"`
}

// FileEntry is one element of the mandatory file index (§6.4).
type FileEntry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Hash string `json:"hash"`
	Mode string `json:"mode"`
}

// EngineManifest is the subset of engine.manifest.json the packager must read to
// check cross-document agreement (§4.6). It validates against the full schema as
// well; this struct only carries the fields that are checked.
type EngineManifest struct {
	Schema        string `json:"schema"`
	Release       string `json:"release"`
	EngineVersion string `json:"engine_version"`
	Wasm          string `json:"wasm"`
	Glue          string `json:"glue"`
	SaveVersion   int    `json:"save_version"`
}

// assetTypes maps an extension to the schema's `type` enum. An unknown
// extension is reported as "other" rather than guessed at.
var assetTypes = map[string]string{
	".png": "texture", ".webp": "texture", ".jpg": "texture", ".jpeg": "texture",
	".ogg": "audio", ".mp3": "audio", ".wav": "audio",
	".obj": "model", ".gltf": "model", ".glb": "model",
	".json": "data", ".csv": "data", ".pak": "data",
	".glsl": "shader", ".wgsl": "shader",
	".mp4": "video", ".webm": "video",
	".ttf": "font", ".otf": "font", ".woff2": "font",
	".css": "other", ".js": "other", ".html": "other", ".txt": "other",
}

// GenerateAssetManifest walks game/assets and writes asset.manifest.json.
func GenerateAssetManifest(stage, gameName, release, generated string, log *Logger) (*AssetManifest, error) {
	assetsRoot := filepath.Join(stage, gameName, "game", "assets")
	files, err := RelFiles(assetsRoot)
	if err != nil {
		return nil, Fail(EvManifestGen, "the asset tree could not be listed", map[string]any{"kind": "asset-manifest"})
	}

	assets := make([]Asset, 0, len(files))
	for _, rel := range files {
		if err := CheckRelPath(rel, []string{""}); err != nil {
			return nil, Fail(EvScanReject, "an asset path is not a safe relative path",
				map[string]any{"path": rel, "reason": err.Error()})
		}
		full := filepath.Join(assetsRoot, filepath.FromSlash(rel))
		hash, size, err := HashFile(full)
		if err != nil {
			return nil, Fail(EvManifestGen, "an asset could not be hashed", map[string]any{"path": rel})
		}
		assets = append(assets, Asset{
			Path:    rel,
			Type:    assetTypes[filepath.Ext(rel)],
			Size:    size,
			Hash:    hash,
			Mutable: true,
			Cache:   "revalidate",
		})
	}
	// An unknown extension maps to "other"; fill it in rather than leaving "".
	for i := range assets {
		if assets[i].Type == "" {
			assets[i].Type = "other"
		}
	}

	manifest := &AssetManifest{
		Schema:    "kobra.asset-manifest/1",
		Release:   release,
		Generated: generated,
		Assets:    assets,
	}
	path := filepath.Join(assetsRoot, "asset.manifest.json")
	if err := WriteJSON(path, manifest); err != nil {
		return nil, Fail(EvManifestGen, "asset.manifest.json could not be written", map[string]any{"kind": "asset-manifest"})
	}
	log.Info(EvManifestGen, map[string]any{
		"kind": "asset-manifest", "entries": len(assets), "bytes": fileSize(path),
	})
	return manifest, nil
}

// IndexEntry is one path the release will carry.
type IndexEntry struct {
	Path string
	Size int64
	Hash string
	Mode string
}

// BuildFileIndex enumerates game/ and launcher/, excluding the manifest itself.
//
// The index and the update archive are built from this one function, which is
// what guarantees they agree: every archive member is indexed and no indexed
// path is missing from the archive.
func BuildFileIndex(stage, gameName, manifestRel string) ([]FileEntry, int64, error) {
	root := filepath.Join(stage, gameName)
	var entries []FileEntry
	var total int64

	for _, namespace := range []string{"game", "launcher"} {
		base := filepath.Join(root, namespace)
		if st, err := os.Stat(base); err != nil || !st.IsDir() {
			continue
		}
		files, err := RelFiles(base)
		if err != nil {
			return nil, 0, Fail(EvManifestGen, "a namespace could not be listed",
				map[string]any{"namespace": namespace})
		}
		for _, rel := range files {
			key := namespace + "/" + rel
			if key == manifestRel {
				// §6.4: the manifest lists every shipped file except itself.
				continue
			}
			full := filepath.Join(base, filepath.FromSlash(rel))
			hash, size, err := HashFile(full)
			if err != nil {
				return nil, 0, Fail(EvManifestGen, "a file could not be hashed", map[string]any{"path": key})
			}
			entries = append(entries, FileEntry{
				Path: key, Size: size, Hash: hash, Mode: ModeString(full),
			})
			total += size
		}
	}

	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		paths = append(paths, e.Path)
	}
	if err := CheckFileIndex(paths); err != nil {
		return nil, 0, Fail(EvScanReject, "the file index is not safe: "+err.Error(),
			map[string]any{"reason": "rel_path"})
	}
	return entries, total, nil
}

// ReleaseManifestParams collects everything GenerateReleaseManifest needs, so
// the call site reads as a list of the facts that go into the document.
type ReleaseManifestParams struct {
	Spec          ReleaseSpec
	LauncherMin   string
	EngineVersion string
	SaveVersion   int
	GameVersion   string
	Archive       *ArchiveRef
}

// releaseManifestDoc assembles the release manifest document and touches no
// filesystem. It is the one place the document is built, so `check` can validate
// it (and with it the whole tree the index describes) without writing an
// archive, and `build` writes exactly what `check` validated.
func releaseManifestDoc(p ReleaseManifestParams, index []FileEntry, totalSize int64) *ReleaseManifest {
	manifest := &ReleaseManifest{
		Schema:          "kobra.release-manifest/1",
		Release:         p.Spec.Release,
		Published:       p.Spec.Generated,
		LauncherMin:     p.LauncherMin,
		EngineVersion:   p.EngineVersion,
		SaveVersion:     p.SaveVersion,
		GameVersion:     p.GameVersion,
		Channel:         orDefault(p.Spec.Channel, "stable"),
		Scope:           orDefault(p.Spec.Scope, "full"),
		TotalSize:       totalSize,
		Archive:         p.Archive,
		Files:           index,
		Migrations:      []any{},
		Notes:           p.Spec.Notes,
		ReleaseNotesURL: p.Spec.NotesURL,
	}
	if p.Spec.PreviousRelease != "" {
		previous := p.Spec.PreviousRelease
		manifest.PreviousRelease = &previous
	}
	return manifest
}

// GenerateReleaseManifest writes game/release.manifest.json into the staged
// tree and returns the document.
//
// It runs AFTER the update archive is built, so `archive` can be filled in and
// the manifest never becomes a member of the archive it describes.
func GenerateReleaseManifest(stage, gameName string, p ReleaseManifestParams,
	index []FileEntry, totalSize int64, log *Logger) (*ReleaseManifest, error) {

	manifest := releaseManifestDoc(p, index, totalSize)

	path := filepath.Join(stage, gameName, "game", "release.manifest.json")
	if err := WriteJSON(path, manifest); err != nil {
		return nil, Fail(EvManifestGen, "release.manifest.json could not be written",
			map[string]any{"kind": "release-manifest"})
	}
	log.Info(EvManifestGen, map[string]any{
		"kind": "release-manifest", "entries": len(index), "bytes": totalSize,
	})
	return manifest, nil
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}
