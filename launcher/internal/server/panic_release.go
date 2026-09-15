//go:build !kobra_dev

package server

// repanicOnPanic is false in a release build: a recovered panic is logged,
// answered with 500 io_error, and turned into a drain that main reports as exit
// code 4 (FR-SRV-24). See panic_dev.go for the development build.
const repanicOnPanic = false
