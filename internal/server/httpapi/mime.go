package httpapi

import (
	"path"
	"strings"
)

// mimeTypes maps the file extensions of the built SPA to their Content-Type (04 §9.5). The table is explicit because
// the distroless image has no /etc/mime.types, so mime.TypeByExtension would answer differently there than on a
// developer machine. The SPA handler never consults the system table.
var mimeTypes = map[string]string{
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".html":        "text/html; charset=utf-8",
	".json":        "application/json",
	".webmanifest": "application/manifest+json",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".ico":         "image/x-icon",
	".webp":        "image/webp",
	".woff2":       "font/woff2",
	".wasm":        "application/wasm",
	".txt":         "text/plain; charset=utf-8",
}

// defaultMIMEType is the Content-Type of a file whose extension is not in mimeTypes. With nosniff, browsers neither
// run nor render it.
const defaultMIMEType = "application/octet-stream"

// contentTypeOf returns the Content-Type for a file name by its extension (case-insensitive).
func contentTypeOf(name string) string {
	if t, ok := mimeTypes[strings.ToLower(path.Ext(name))]; ok {
		return t
	}
	return defaultMIMEType
}

// isHTMLType reports whether a Content-Type value is HTML.
func isHTMLType(contentType string) bool {
	return strings.HasPrefix(strings.ToLower(contentType), "text/html")
}
