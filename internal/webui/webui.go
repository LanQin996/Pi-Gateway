// Package webui serves the embedded admin single-page application.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// dist holds the built Vue application.
//
// The directory is committed with a placeholder so `go build` works before the
// frontend has been compiled; run `npm run build` in web/ to populate it.
//
//go:embed all:dist
var dist embed.FS

// Handler serves the SPA, falling back to index.html for client-side routes.
const cacheControlHeader = "Cache-Control"

func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "web UI assets are unavailable", http.StatusInternalServerError)
		})
	}

	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}

		requested, valid := assetPath(r.URL.Path)
		if !valid {
			http.NotFound(w, r)
			return
		}
		if requested == "" || requested == "." {
			serveIndex(w, r, sub)
			return
		}

		// Serve real assets directly; anything else is a client-side route.
		if f, err := sub.Open(requested); err == nil {
			_ = f.Close()
			if !strings.HasPrefix(requested, "assets/") {
				// index.html must never be cached or the SPA will pin to an old build.
				w.Header().Set(cacheControlHeader, "no-cache")
			} else {
				w.Header().Set(cacheControlHeader, "public, max-age=31536000, immutable")
			}
			fileServer.ServeHTTP(w, r)
			return
		}
		serveIndex(w, r, sub)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, sub fs.FS) {
	raw, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.Error(w, "web UI is not built yet. Run `npm install && npm run build` in the web/ directory.", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set(cacheControlHeader, "no-cache")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(raw)
}

// assetPath only admits paths within the embedded filesystem. Reject traversal
// before cleaning rather than silently normalizing it into another asset.
func assetPath(urlPath string) (string, bool) {
	if strings.ContainsAny(urlPath, "\\\x00") {
		return "", false
	}
	for _, segment := range strings.Split(urlPath, "/") {
		if segment == ".." {
			return "", false
		}
	}
	name := strings.TrimPrefix(path.Clean(urlPath), "/")
	return name, name == "" || fs.ValidPath(name)
}
