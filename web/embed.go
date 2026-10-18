package web

import "embed"

//go:embed *.html *.css *.js *.svg
var FS embed.FS
