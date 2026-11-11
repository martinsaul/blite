package server

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

//go:embed terms.html
var termsHTML []byte

//go:embed privacy.html
var privacyHTML []byte

//go:embed download.html
var downloadHTML string

//go:embed site
var siteFiles embed.FS

//go:embed docs/*.html
var docsFiles embed.FS

// siteURL is the canonical origin for the public pages.
const siteURL = "https://bellion.app"

// canonicalHost redirects page requests on Config.SiteAliases to siteURL, so
// search engines index one name. Only pages: the app may still use /ws and
// /v1 on those hosts.
func (s *Server) canonicalHost(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if slices.Contains(s.cfg.SiteAliases, strings.ToLower(r.Host)) {
			http.Redirect(w, r, siteURL+r.URL.RequestURI(), http.StatusMovedPermanently)
			return
		}
		h(w, r)
	}
}

// siteFile serves an embedded icon or image.
func siteFile(name, ctype string) http.HandlerFunc {
	b, err := siteFiles.ReadFile("site/" + name)
	if err != nil {
		panic(err)
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(b)
	}
}

// siteShots are the app screenshots on the download page and in the docs,
// served at /static/shots/<name>.
var siteShots = []string{"call.webp", "lobby.webp", "lobby-discord.webp", "lobby-watch.webp", "share-menu.webp", "people-menu.webp"}

// docPage is one page of the docs section.
type docPage struct {
	Path, Label, File string
}

var docPages = []docPage{
	{"/docs", "Overview", "index.html"},
	{"/docs/getting-started", "Getting started", "getting-started.html"},
	{"/docs/features", "Features", "features.html"},
	{"/docs/discord", "Discord", "discord.html"},
	{"/docs/relays", "Relays", "relays.html"},
	{"/docs/self-hosting", "Self-hosting", "self-hosting.html"},
	{"/docs/troubleshooting", "Troubleshooting", "troubleshooting.html"},
}

// renderDocs renders every docs page once: they don't change at runtime.
func renderDocs() map[string][]byte {
	out := map[string][]byte{}
	funcs := template.FuncMap{"hasPrefix": strings.HasPrefix}
	for _, p := range docPages {
		t := template.Must(template.New("").Funcs(funcs).ParseFS(docsFiles, "docs/_layout.html", "docs/"+p.File))
		var b strings.Builder
		if err := t.ExecuteTemplate(&b, "layout", map[string]any{"Path": p.Path, "Pages": docPages}); err != nil {
			panic(err)
		}
		out[p.Path] = []byte(b.String())
	}
	return out
}

var docsHTML = renderDocs()

const robotsTxt = `User-agent: *
Disallow: /j/
Disallow: /v1/
Disallow: /download/

Sitemap: ` + siteURL + `/sitemap.xml
`

// sitemapXML lists the public pages.
var sitemapXML = func() string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
	paths := []string{"/"}
	for _, p := range docPages {
		paths = append(paths, p.Path)
	}
	for _, p := range append(paths, "/terms", "/privacy") {
		b.WriteString("<url><loc>" + siteURL + p + "</loc></url>\n")
	}
	b.WriteString("</urlset>\n")
	return b.String()
}()

func textFile(body, ctype string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Write([]byte(body))
	}
}

var downloadTmpl = template.Must(template.New("download").Parse(downloadHTML))

func staticPage(b []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.Write(b)
	}
}

// release is one platform's latest build on the update server.
type release struct {
	Platform string    `json:"platform"`
	Build    int       `json:"build"`
	Filename string    `json:"filename"`
	Size     int64     `json:"size"`
	URL      string    `json:"url"`
	Released time.Time `json:"releasedAt"`
}

// releases caches the latest release per platform for the download page.
type releases struct {
	base, channel string
	mu            sync.Mutex
	at            time.Time
	byPlatform    map[string]*release
}

var downloadPlatforms = []string{"windows-amd64", "linux-amd64"}

// get returns the latest releases, refetched at most every 5 minutes. The
// manifests are signed for the app; this is only a link, so the page doesn't
// check signatures (the app does, on every update).
func (r *releases) get(ctx context.Context) map[string]*release {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byPlatform != nil && time.Since(r.at) < 5*time.Minute {
		return r.byPlatform
	}
	m := map[string]*release{}
	c := http.Client{Timeout: 10 * time.Second}
	for _, p := range downloadPlatforms {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/v1/manifest/blite/%s/%s", strings.TrimRight(r.base, "/"), r.channel, p), nil)
		res, err := c.Do(req)
		if err != nil {
			continue
		}
		var env struct{ Payload string }
		err = json.NewDecoder(res.Body).Decode(&env)
		res.Body.Close()
		if err != nil || res.StatusCode != http.StatusOK {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(env.Payload)
		var rel release
		if err != nil || json.Unmarshal(b, &rel) != nil || !strings.HasPrefix(rel.URL, "https://") {
			continue
		}
		m[p] = &rel
	}
	if len(m) > 0 || r.byPlatform == nil {
		r.byPlatform, r.at = m, time.Now()
	}
	return r.byPlatform
}

func (s *Server) handleDownloadPage(w http.ResponseWriter, r *http.Request) {
	rel := s.releases.get(r.Context())
	type dl struct {
		Build    int
		Size     string
		Released string
	}
	data := map[string]*dl{}
	for p, x := range rel {
		data[strings.TrimSuffix(p, "-amd64")] = &dl{x.Build, fmt.Sprintf("%.0f MB", float64(x.Size)/1e6), x.Released.Format("January 2, 2006")}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=60")
	downloadTmpl.Execute(w, data)
}

// handleDownload sends the latest build for a platform (windows or linux).
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	x := s.releases.get(r.Context())[r.PathValue("platform")+"-amd64"]
	if x == nil {
		http.Error(w, "no release available right now; try again later", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	http.Redirect(w, r, x.URL, http.StatusFound)
}
