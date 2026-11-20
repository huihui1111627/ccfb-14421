// Package web holds the single-page inspector UI and embeds it into the
// server binary, so the binary runs from anywhere.
package web

import "embed"

//go:embed index.html
var FS embed.FS
