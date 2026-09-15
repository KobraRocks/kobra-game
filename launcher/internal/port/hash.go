package port

// Source records where an allocation candidate came from. The values are the
// strings the Appendix C.2 port.candidate event documents.
type Source string

const (
	// SourceSaved is a port read back from the sidecar's port.json (§8.1).
	SourceSaved Source = "saved"
	// SourceDefault is the deterministic hash-derived port (§7.3).
	SourceDefault Source = "default"
	// SourceScan is a port found by Scan or WideScan (§8.3).
	SourceScan Source = "scan"
)

// FNV-1a 32 constants from the FNV specification: offset basis 2166136261,
// prime 16777619.
const (
	fnvOffset32 uint32 = 2166136261
	fnvPrime32  uint32 = 16777619
)

// FNV1a32 is the standard 32-bit FNV-1a hash over the UTF-8 bytes of s
// (Launcher spec §7.3, §26.3). It is implemented locally rather than taken
// from hash/fnv so the exact byte sequence is obvious and cannot drift with a
// standard-library change.
//
// The result is pinned by a golden test: fnv1a32("com.kobra.stardrifter") is a
// fixed value, and an implementation change that moves it would silently move
// every existing install to a new origin (§26.3).
func FNV1a32(s string) uint32 {
	h := fnvOffset32
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= fnvPrime32
	}
	return h
}

// Default is the deterministic default port: base + fnv1a32(gameID) % span
// (§7.3, §8.1). It is offered to the user as the proposed port, and is the
// fallback when no sidecar port.json exists.
//
// A span of 0 would divide by zero; callers validate port.span >= 1 (config
// does), and Default degrades to base rather than panicking.
func Default(gameID string, base, span uint16) uint16 {
	if span == 0 {
		return base
	}
	return uint16(uint32(base) + FNV1a32(gameID)%uint32(span))
}
