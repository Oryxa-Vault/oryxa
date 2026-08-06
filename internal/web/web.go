// Package web serves the viewer, embedded in the binary.
//
// Embedded rather than a separate app: the whole premise is one self-hosted
// binary, and a viewer that needs its own deploy would undo that.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed assets
var assets embed.FS

// Handler serves the viewer at the path it is mounted on.
func Handler() http.Handler {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The viewer is one page; any unknown path renders it rather than 404ing.
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			if _, err := fs.Stat(sub, r.URL.Path[1:]); err != nil {
				r = r.Clone(r.Context())
				r.URL.Path = "/"
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		files.ServeHTTP(w, r)
	})
}
