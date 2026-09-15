//go:build !kobra_faultinject

package faultinject

import "testing"

// TestReleaseBuildCarriesNoInjection is the guard on the build tag: a default
// build must not be able to inject anything, no matter what the environment
// says. Every seam is a no-op there, which is what makes it safe to leave the
// call sites in production code.
func TestReleaseBuildCarriesNoInjection(t *testing.T) {
	t.Setenv("KOBRA_FAULT", "storage.write.after_fsync=exit:70,server.handler=panic,storage.rename=exdev")

	if Enabled() {
		t.Error("Enabled() is true in a build without the kobra_faultinject tag")
	}
	for _, p := range []string{StorageWrite, StorageRename, UpdatePromoteRename} {
		if err := Err(p); err != nil {
			t.Errorf("Err(%s) = %v, want nil in a release build", p, err)
		}
	}
	// Neither of these may terminate the process or panic.
	Point(StorageWriteAfterFsync)
	Point(StorageWriteAfterRename)
	Point(ServerHandler)
}
