// Package ui provides embedded HTML templates and static assets for the
// log-explorer web interface.
package ui

import (
	"embed"
	"io/fs"
)

//go:embed templates/*.html
var Templates embed.FS

//go:embed static
var staticFS embed.FS

// Static exposes the embedded static assets (CSS) as a file system rooted at
// the static/ directory, so http.FileServer can resolve style.css directly.
var Static = func() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // embedded static directory always exists
	}
	return sub
}()
