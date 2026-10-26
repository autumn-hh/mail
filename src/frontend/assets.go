// Package frontend embeds the existing templates and static assets in the Go binary.
package frontend

import "embed"

//go:embed templates/*.html static/styles/*.css static/scripts/*.js static/icons/*.svg
var Files embed.FS
