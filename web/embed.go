// Package web embeds the built control-panel SPA (web/dist) into the
// controller binary. Run `make web` before `make build` to include a fresh
// UI; without a build, the placeholder below is served.
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
