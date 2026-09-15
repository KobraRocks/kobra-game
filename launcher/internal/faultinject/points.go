// Package faultinject provides the injection points of Launcher spec §26.4.
//
// It exists in two forms:
//
//   - built without the kobra_faultinject tag (every release build), Point is a
//     no-op and Err always returns nil, so the seams cost nothing and cannot
//     fire;
//   - built with the tag, an armed plan is read from KOBRA_FAULT and the named
//     points crash the process, panic, or manufacture the platform error a
//     scenario needs.
//
// Call sites are unconditional, which is what keeps them honest: the same line
// of production code runs in both builds, so a test cannot pass because it took
// a different branch from the shipped one.
//
// KOBRA_FAULT is a comma-separated list of point=action pairs, for example:
//
//	KOBRA_FAULT='storage.write.after_fsync=exit:70,server.handler=panic'
//
// Actions:
//
//	exit:N  stop the process with status N, simulating a crash
//	panic   panic on the calling goroutine
//	exdev   return a synthetic cross-device error from Err at that point
//	enospc  return a synthetic out-of-space error from Err at that point
//
// Each point fires at most once per process, so a retry or a loop cannot turn a
// single injected fault into a storm.
package faultinject

// Injection point names. They are constants rather than literals so a test and
// the call site cannot silently disagree about a string.
const (
	// StorageWrite fires where writeAtomic has written the payload but not yet
	// synced it. Armed with enospc it models a disk that fills mid-write.
	StorageWrite = "storage.write"
	// StorageWriteAfterFsync fires after the payload is durable and before the
	// previous revision is rotated to .bak. Armed with exit it models a crash
	// in that window.
	StorageWriteAfterFsync = "storage.write.after_fsync"
	// StorageWriteAfterRename fires after the new revision is in place and
	// before the containing directory is fsynced. Armed with exit it models a
	// crash whose durability depends on the directory entry.
	StorageWriteAfterRename = "storage.write.after_rename"
	// StorageRename fires where the temp file is renamed onto the target; armed
	// with exdev it forces the cross-device fallback without needing two
	// filesystems.
	StorageRename = "storage.rename"
	// UpdatePromoteRename fires where a staged release is promoted into game/;
	// armed with exdev it forces promoteDir's copy path.
	UpdatePromoteRename = "update.promote.rename"
	// ServerHandler fires on every request, which is where an injected panic
	// exercises the recovery middleware and, in a real process, exit code 4.
	ServerHandler = "server.handler"
)
