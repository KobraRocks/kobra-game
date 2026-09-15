//go:build kobra_dev

package server

// repanicOnPanic makes a development build re-panic after it has logged a
// handler panic, so the developer sees the real failure with its stack instead
// of a drained, unremarkable exit. Release builds drain and exit 4
// (FR-SRV-24); see panic_release.go.
const repanicOnPanic = true
