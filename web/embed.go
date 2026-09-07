// Package web embeds the built control-panel SPA (web/dist) into the
// controller binary. The repository's dependency-free console is checked in
// under web/dist, so normal controller builds always carry the current panel.
package web

import (
	"embed"
	"io/fs"
)

//go:embed dist
var dist embed.FS

// Dist returns the built SPA as a filesystem rooted at the app's index.
func Dist() (fs.FS, error) {
	return fs.Sub(dist, "dist")
}
