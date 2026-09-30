// Package web embeds the built single-page app (the React client under web/src, built by Vite into web/dist) in the
// isshoni binary. It is a Go package inside web/ because go:embed cannot reach parent directories.
//
// The repository commits only dist/.gitkeep, so the package builds on a fresh clone without Node; `task build:web`
// fills dist/ (index.html, hashed assets/, version.json, .br and .gz siblings, …) before `task build:go`. The server's
// SPA handler serves a "web UI not built" page when index.html is missing.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist returns the built SPA rooted at dist/: index.html is "index.html", not "dist/index.html".
func Dist() fs.FS {
	sub, _ := fs.Sub(dist, "dist") // fs.Sub fails only on an invalid path, and "dist" is valid.
	return sub
}
