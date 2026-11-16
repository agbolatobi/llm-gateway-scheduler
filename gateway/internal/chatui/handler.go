// Package chatui serves the browser client. API authentication stays in api.
package chatui

import (
	"embed"
	"net/http"
	"strconv"
)

const Path = "/chat/"

//go:embed static/*
var assets embed.FS

var publicAssets = map[string]struct{ file, contentType string }{
	Path:                            {"index.html", "text/html; charset=utf-8"},
	Path + "styles.css":             {"styles.css", "text/css; charset=utf-8"},
	Path + "app.mjs":                {"app.mjs", "text/javascript; charset=utf-8"},
	Path + "session.mjs":            {"session.mjs", "text/javascript; charset=utf-8"},
	Path + "stream.mjs":             {"stream.mjs", "text/javascript; charset=utf-8"},
	Path + "markdown.mjs":           {"markdown.mjs", "text/javascript; charset=utf-8"},
	Path + "vendor/markdown-it.mjs": {"vendor/markdown-it.mjs", "text/javascript; charset=utf-8"},
}

// Serve handles only the explicitly listed public assets. Loading the page grants
// no access to models or inference; both still require the user's Bearer token.
func Serve(responseWriter http.ResponseWriter, request *http.Request) bool {
	asset, exists := publicAssets[request.URL.Path]
	if !exists && request.URL.Path != "/chat" {
		return false
	}
	headers := responseWriter.Header()
	headers.Set("Cache-Control", "no-store")
	headers.Set("X-Content-Type-Options", "nosniff")
	headers.Set("X-Frame-Options", "DENY")
	headers.Set("Referrer-Policy", "no-referrer")
	headers.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		headers.Set("Allow", "GET, HEAD")
		http.Error(responseWriter, "Use GET or HEAD.", http.StatusMethodNotAllowed)
		return true
	}
	if request.URL.Path == "/chat" {
		http.Redirect(responseWriter, request, Path, http.StatusPermanentRedirect)
		return true
	}
	content, err := assets.ReadFile("static/" + asset.file)
	if err != nil {
		http.Error(responseWriter, "Chat UI unavailable.", http.StatusInternalServerError)
		return true
	}
	headers.Set("Content-Type", asset.contentType)
	headers.Set("Content-Length", strconv.Itoa(len(content)))
	if request.Method != http.MethodHead {
		responseWriter.Write(content)
	}
	return true
}
