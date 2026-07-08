// Package web embeds the built frontend.
package web

import (
	"embed"
	"io/fs"
)

// all: includes dist/.gitkeep, so this compiles before the frontend is built.
//
//go:embed all:dist
var dist embed.FS

// Assets returns the built frontend rooted at dist/.
func Assets() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
