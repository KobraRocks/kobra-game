package pack

import _ "embed"

// thirdPartyNotices is the notice set that must travel with every package.
//
// A game package ships launcher/launcher, a statically linked binary containing
// BSD-3-Clause (klauspost/compress, golang.org/x/sys) and Apache-2.0
// (santhosh-tekuri/jsonschema) code. Those licences require their notices to
// accompany the distribution, so the packager writes this file into the package's
// LICENSES/ directory alongside a README that points at it.
//
// It is an embedded COPY of the repository's THIRD_PARTY_NOTICES.md, because
// kobra-pack runs from a publisher's machine and cannot read the source tree.
// TestEmbeddedNoticesMatchRepository fails if the copy drifts.
//
//go:embed notices/THIRD_PARTY_NOTICES.md
var thirdPartyNotices []byte
