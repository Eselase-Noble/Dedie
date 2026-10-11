//go:build !noui

// Package web carries the built web interface inside the Go binaries.
//
// Run `npm run build` in this directory first; it writes the static export to
// ./out, which is embedded below. To compile without Node installed (for
// example to run the Go tests), build with `-tags noui`.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:out
var files embed.FS

// UI returns the exported site, or nil when it was not built in.
func UI() fs.FS {
	sub, err := fs.Sub(files, "out")
	if err != nil {
		return nil
	}
	return sub
}
