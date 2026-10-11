//go:build noui

package web

import "io/fs"

// UI returns nil: this build carries the API without the web interface.
func UI() fs.FS { return nil }
