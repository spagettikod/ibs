// Package web embeds the built React admin UI.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Assets returns the built UI files (empty until `npm run build` has run).
func Assets() fs.FS {
	sub, _ := fs.Sub(dist, "dist")
	return sub
}
