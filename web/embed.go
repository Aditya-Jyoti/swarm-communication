// Package web holds the dashboard's static assets and embeds them into the
// control-center binary.
//
// Embedding, rather than serving files from disk, makes the Control Center a
// single self-contained binary: the container image needs no asset directory,
// and `go run ./cmd/control-center` works from any working directory.
package web

import (
	"embed"
	"io/fs"
)

// static is the raw embedded tree, rooted at "static/...".
//
// The "all:" prefix includes dot-files, which is what lets the placeholder
// static/.gitkeep satisfy the directive before the dashboard files exist: a
// //go:embed pattern that matches nothing is a compile error.
//
//go:embed all:static
var static embed.FS

// Static returns the dashboard assets rooted at static/, so the page is
// "index.html", not "static/index.html". It cannot fail at runtime: the tree is
// fixed at compile time, so an error here is a build defect, hence the panic.
func Static() fs.FS {
	sub, err := fs.Sub(static, "static")
	if err != nil {
		panic("web: embedded static tree is missing: " + err.Error())
	}
	return sub
}
