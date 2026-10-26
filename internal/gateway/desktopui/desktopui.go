// Package desktopui is the gateway's built-in desktop viewer (G15): one
// self-contained HTML page, with inline CSS and JavaScript and no external
// URLs, that speaks RFB 3.8 to the gateway over a WebSocket at Path,
// resolved relative to the page's own URL. The gateway serves the page and
// proxies the RFB stream; it always offers security type None, so the page
// has no authentication code of its own.
//
// View-only mode is set on the page's <html> element and only hides the
// input path in the browser; the gateway enforces it independently.
package desktopui

import (
	"bytes"
	_ "embed"
)

// Path is the WebSocket endpoint the page connects to, relative to the
// directory the page is served from ("websockify" next to "/").
const Path = "/websockify"

//go:embed viewer.html
var page []byte

var (
	viewOnlyOff = []byte(`data-view-only="false"`)
	viewOnlyOn  = []byte(`data-view-only="true"`)
)

// Page returns the viewer page. With viewOnly set the <html> element carries
// data-view-only="true", and the page sends no key or pointer input. The
// result is a fresh copy the caller may keep or modify.
func Page(viewOnly bool) []byte {
	if !viewOnly {
		return bytes.Clone(page)
	}
	return bytes.Replace(page, viewOnlyOff, viewOnlyOn, 1)
}
