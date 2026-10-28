package uiservice

import (
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
)

// The app's page: one HTML shell plus static CSS and ES modules, all built
// into the binary. Everything the page shows comes from the API.

//go:embed web
var webFS embed.FS

var (
	pageTemplate = template.Must(template.ParseFS(webFS, "web/index.html"))
	// iconSprite is inlined into the page, so icons need no request.
	iconSprite = template.HTML(mustReadWeb("web/icons.svg")) // #nosec G203 -- static, built-in file
)

func mustReadWeb(name string) string {
	b, err := webFS.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// pageCSP lets the page load scripts, styles and data from this service
// only; nothing inline runs, which is why the token rides in a data
// attribute rather than a script.
const pageCSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; " +
	"frame-ancestors 'none'; base-uri 'none'; form-action 'none'; object-src 'none'"

func pageHeaders(h http.Header) {
	h.Set("Content-Security-Policy", pageCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
}

type pageData struct {
	Token, InstanceID, Version, Address string
	Sprite                              template.HTML
}

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	pageHeaders(w.Header())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = pageTemplate.Execute(w, pageData{
		Token: s.token, InstanceID: s.info.InstanceID, Version: s.info.Version, Address: s.info.HTTPAddr, Sprite: iconSprite,
	})
}

// assets serves /assets/css/… and /assets/js/…. A new ccwrap is a new
// service instance (the page reloads), so files are revalidated, not cached.
func assets() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	files := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/assets/")
		if !(strings.HasPrefix(name, "css/") || strings.HasPrefix(name, "js/")) || strings.HasSuffix(name, "/") {
			http.NotFound(w, r)
			return
		}
		pageHeaders(w.Header())
		w.Header().Set("Cache-Control", "no-cache")
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/" + name
		files.ServeHTTP(w, r2)
	})
}
