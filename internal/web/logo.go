package web

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// The aggregators' logos, each taken from the aggregator's own website (the
// README lists where), as static/logos/{id}.png or .svg: square tiles the page
// rounds. An aggregator without one is shown with its monogram.
//
//go:embed static
var staticFS embed.FS

// logos maps an aggregator to its logo's URL.
var logos = func() map[string]string {
	m := map[string]string{}
	entries, err := fs.ReadDir(staticFS, "static/logos")
	if err != nil {
		panic(err)
	}
	for _, e := range entries {
		m[strings.TrimSuffix(e.Name(), path.Ext(e.Name()))] = "/static/logos/" + e.Name()
	}
	return m
}()

// avatar is an aggregator's logo, or its monogram on its color; size is "" or
// "lg".
func avatar(id, size string) template.HTML {
	class := strings.TrimSpace("av " + size)
	if src, ok := logos[id]; ok {
		return template.HTML(fmt.Sprintf(`<span class="%s logo"><img src="%s" alt="" width="48" height="48"></span>`, class, src))
	}
	return template.HTML(fmt.Sprintf(`<span class="%s" style="--c: %s">%s</span>`,
		class, color(id), template.HTMLEscapeString(monogram(id))))
}

// asset is the URL of a file in static/ with its content's hash: a change
// reaches browsers and Cloudflare at once, though both keep the file a day.
func asset(name string) string {
	if u, ok := assets[name]; ok {
		return u
	}
	panic("no static file " + name)
}

var assets = func() map[string]string {
	m := map[string]string{}
	err := fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := staticFS.ReadFile(p)
		sum := sha256.Sum256(b)
		m[strings.TrimPrefix(p, "static/")] = fmt.Sprintf("/%s?v=%x", p, sum[:6])
		return err
	})
	if err != nil {
		panic(err)
	}
	return m
}()

// static serves the embedded files under /static/, without directory listings.
func static() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	files := http.StripPrefix("/static", http.FileServerFS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=86400")
		files.ServeHTTP(w, r)
	})
}
