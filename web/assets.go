package web

import "embed"

// Assets contains the browser assets. templ components compile directly into
// Go; no JavaScript toolchain or frontend build is required.
//
//go:embed assets favicon.svg
var Assets embed.FS
