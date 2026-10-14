package web

import (
	"embed"
	"io/fs"
)

// Assets contains the production frontend distribution. The committed
// .gitkeep keeps `go test ./...` working in a fresh checkout; NewHandler
// refuses to serve it until the frontend build has produced index.html.
//
//go:embed all:dist
var Assets embed.FS

// HasPlaceholderAssets reports whether the source-tree scaffold has not yet
// been replaced with a frontend build.
func HasPlaceholderAssets() bool {
	_, err := fs.Stat(Assets, "dist/index.html")
	return err != nil
}
