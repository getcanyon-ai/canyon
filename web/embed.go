// Package web carries the built dashboard into the binary. `make web` fills dist; a build
// without it embeds only the committed marker, and the server then serves no dashboard at all
// rather than half of one.
package web

import "embed"

//go:embed all:dist
var Dist embed.FS
