// Package web embeds the built console SPA. Run `npm run build` in this
// directory (or use the Dockerfile) to produce dist/.
package web

import "embed"

//go:embed all:dist
var Dist embed.FS
