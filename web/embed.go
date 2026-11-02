// Package web embeds the built portal, so otter ships as one binary. It
// lives beside dist/ because //go:embed can't reach outside its directory.
package web

import (
	"embed"
	"io/fs"
)

// all: because dist/ holds only .gitkeep until the portal is built, and
// embed skips dotfiles otherwise.
//
//go:embed all:dist
var dist embed.FS

// Assets is the build output, rooted at dist/. It has no index.html when
// the portal wasn't built (make web).
func Assets() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // dist is embedded at compile time
	}
	return sub
}
