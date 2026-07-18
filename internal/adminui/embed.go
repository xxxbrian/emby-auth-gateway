// Package adminui embeds the admin SPA static assets.
package adminui

import "embed"

// Dist is the built admin UI (placeholder until Svelte assets are committed).
//
//go:embed all:dist
var Dist embed.FS
