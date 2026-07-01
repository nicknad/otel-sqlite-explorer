// Package ui provides embedded HTML templates for the log-explorer web interface.
package ui

import "embed"

//go:embed templates/*.html
var Templates embed.FS
