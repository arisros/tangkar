package web

import "embed"

//go:embed *.html *.css *.js *.svg fonts/*.woff2
var FS embed.FS
