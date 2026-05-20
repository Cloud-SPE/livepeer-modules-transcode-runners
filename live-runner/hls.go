package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func hlsHandler(root, basePath string) http.Handler {
	fs := http.FileServer(http.Dir(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, basePath+"/") {
			http.NotFound(w, r)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, basePath+"/")
		clean := filepath.Clean(path)
		if clean == "." || strings.HasPrefix(clean, "..") {
			http.NotFound(w, r)
			return
		}
		full := filepath.Join(root, clean)
		if _, err := os.Stat(full); err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.StripPrefix(basePath+"/", fs).ServeHTTP(w, r)
	})
}
