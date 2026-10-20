// Package web is the server-rendered webui (KTD10): Go html/template +
// htmx for partial updates, assets embedded via go:embed so the app ships
// as one binary with no separate build toolchain or CDN dependency.
package web

import (
	"embed"
	"fmt"
	"html/template"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var StaticFS embed.FS

// assetVersion is the cache-busting query string appended to /static/app.css
// across every template ({{assetVer}}). Bump it whenever app.css changes so
// browsers refetch. Keeping it in one place stops the per-template ?v= values
// from drifting out of sync (which silently served stale CSS on some pages).
const assetVersion = "11"

// BuildVersion is the deployed build stamp shown on the About page (git
// describe + build date), so operators can confirm what's actually running
// after a deploy. Set at build time via ldflags
// (-X '.../internal/web.BuildVersion=...'); "dev" for a plain `go build`.
var BuildVersion = "dev"

// humanBytes renders a byte count the way an operator wants to read disk
// usage (R2), not the way SQLite stores it.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func parseTemplates() (*template.Template, error) {
	funcs := template.FuncMap{
		"humanBytes":   humanBytes,
		"assetVer":     func() string { return assetVersion },
		"buildVersion": func() string { return BuildVersion },
	}
	return template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html")
}
