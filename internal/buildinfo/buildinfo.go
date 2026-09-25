// Package buildinfo is populated at link time via -ldflags.
package buildinfo

var (
	Version = "dev"
	Commit  = "none"
)
