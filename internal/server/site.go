package server

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

//go:embed chrome.html download.html terms.html privacy.html
var pageFiles embed.FS

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

// siteCSS is the stylesheet every public page shares, served at
// /static/site.css. Pages link it with a content hash (siteCSSURL), so
// browsers can cache it for long and still get a new one after a deploy.
var siteCSS = func() []byte {
	b, err := siteFiles.ReadFile("site/site.css")
	if err != nil {
		panic(err)
	}
	return b
}()

var siteCSSVersion = func() string {
	h := sha256.Sum256(siteCSS)
	return hex.EncodeToString(h[:6])
}()

var siteCSSURL = "/static/site.css?v=" + siteCSSVersion

func handleSiteCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	if r.URL.Query().Get("v") == siteCSSVersion {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=300")
	}
	w.Write(siteCSS)
}

// siteFuncs are the template functions the page chrome (chrome.html) uses.
var siteFuncs = template.FuncMap{
	"hasPrefix": strings.HasPrefix,
	"css":       func() string { return siteCSSURL },
}

// sitePage parses a page from pageFiles together with the shared chrome.
func sitePage(name string) *template.Template {
	return template.Must(template.New(name).Funcs(siteFuncs).ParseFS(pageFiles, "chrome.html", name))
}

// renderPage renders a page that doesn't change at runtime.
func renderPage(name string) []byte {
	var b strings.Builder
	if err := sitePage(name).ExecuteTemplate(&b, name, nil); err != nil {
		panic(err)
	}
	return []byte(b.String())
}

var (
	termsHTML   = renderPage("terms.html")
	privacyHTML = renderPage("privacy.html")
)

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

// tocEntry is one section in a docs page's "On this page" list.
type tocEntry struct {
	ID    string
	Label template.HTML
}

// docSection matches a docs page's section headings: the h2s that start a
// line (cards and the like nest theirs in other elements).
var docSection = regexp.MustCompile(`(?m)^<h2(?: id="([^"]*)")?>(.*?)</h2>`)

var (
	tagRE  = regexp.MustCompile(`<[^>]*>`)
	slugRE = regexp.MustCompile(`[^a-z0-9]+`)
)

// sectionTOC gives every section heading in body an id (a slug of its text,
// unless it has one) and returns the body and its table of contents.
func sectionTOC(body string) (string, []tocEntry) {
	var toc []tocEntry
	seen := map[string]bool{}
	body = docSection.ReplaceAllStringFunc(body, func(h string) string {
		m := docSection.FindStringSubmatch(h)
		id, label := m[1], m[2]
		if id == "" {
			id = strings.Trim(slugRE.ReplaceAllString(strings.ToLower(tagRE.ReplaceAllString(label, "")), "-"), "-")
			for base, n := id, 2; seen[id]; n++ {
				id = fmt.Sprintf("%s-%d", base, n)
			}
		}
		seen[id] = true
		toc = append(toc, tocEntry{id, template.HTML(tagRE.ReplaceAllString(label, ""))})
		return `<h2 id="` + id + `">` + label + `</h2>`
	})
	return body, toc
}

// renderDocs renders every docs page once: they don't change at runtime.
func renderDocs() map[string][]byte {
	out := map[string][]byte{}
	for _, p := range docPages {
		t := template.Must(template.New("").Funcs(siteFuncs).ParseFS(pageFiles, "chrome.html"))
		t = template.Must(t.ParseFS(docsFiles, "docs/_layout.html", "docs/"+p.File))
		var body strings.Builder
		if err := t.ExecuteTemplate(&body, "body", nil); err != nil {
			panic(err)
		}
		html, toc := sectionTOC(body.String())
		if len(toc) < 2 {
			toc = nil
		}
		var b strings.Builder
		data := map[string]any{"Path": p.Path, "Pages": docPages, "Body": template.HTML(html), "TOC": toc}
		if err := t.ExecuteTemplate(&b, "layout", data); err != nil {
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

var downloadTmpl = sitePage("download.html")

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
