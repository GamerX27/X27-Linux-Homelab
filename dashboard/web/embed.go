// Package web holds the browser UI: plain HTML, CSS and ES modules with no build step,
// plus a vendored xterm.js for the terminal.
package web

import "embed"

//go:embed index.html style.css js vendor
var FS embed.FS
