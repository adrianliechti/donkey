// Package web embeds the built admin console so donkey-server ships as a
// single binary. Build the assets with `task web-build` (vite) before
// building the server.
package web

import "embed"

//go:embed all:dist
var Dist embed.FS
