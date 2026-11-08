package web

import "embed"

//go:embed templates/*.html static/* openapi.json
var FS embed.FS
