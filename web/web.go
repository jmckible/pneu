// Package web embeds the browser-side assets served at /static/.
package web

import "embed"

// Static holds static/: CSS and vendored JS (DOMPurify, no CDN).
//
//go:embed static
var Static embed.FS
