package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testAPKDigest is the test APK's SHA-256.
const testAPKDigest = "e4aa4119ddafaf542c65e33d43e075830e846d918bd0650450f586105c2e38fe"

const testAndroidManifest = `{"versionName":"0.1.0","versionCode":3,"build":223,"size":15141375,
"sha256":"` + testAPKDigest + `",
"certSha256":"28:57:9D:C3:73:7F:64:50:E5:4D:3E:6B:58:C2:FD:B9:C2:0E:C8:A5:E8:E1:05:CF:0E:E9:F9:14:A7:19:FC:4D",
"url":"https://bellion.app/download/android/bellion-share-0.1.0-vc3.apk","released":"2026-11-24T04:28:00Z","minAndroid":"10"}`

// androidUpstream serves body (an empty body = 404) and counts the fetches.
func androidUpstream(t *testing.T, body *atomic.Value, n *atomic.Int32) string {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		b := body.Load().(string)
		if b == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(b))
	}))
	t.Cleanup(up.Close)
	return up.URL
}

func TestAndroidDownload(t *testing.T) {
	molt := httptest.NewServer(http.NotFoundHandler())
	defer molt.Close()
	var body atomic.Value
	var fetches atomic.Int32
	body.Store(testAndroidManifest)
	s := New(Config{UpdateURL: molt.URL, UpdateChannel: "stable", AndroidManifest: androidUpstream(t, &body, &fetches) + "/download/android.json"})
	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.Host = "bellion.app"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}

	page := get("/").Body.String()
	for _, want := range []string{
		`href="https://groups.google.com/g/bellion-share-testers">Get it on Google Play (beta)</a>`,
		"https://play.google.com/apps/testing/io.saul.bellion",
		"closed beta that anyone can join",
		`href="/download/android">Download the APK</a>`,
		"Version 0.1.0 (vc 3) &middot; 15 MB &middot; November 24, 2026",
		"signed with the same key",
		testAPKDigest,
		androidCertSHA256,
		"watch-only for now",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("home page: missing %q", want)
		}
	}
	w := get("/download/android")
	if w.Code != http.StatusFound || w.Header().Get("Location") != "https://bellion.app/download/android/bellion-share-0.1.0-vc3.apk" {
		t.Fatalf("/download/android: %d %q", w.Code, w.Header().Get("Location"))
	}
	get("/download")
	if n := fetches.Load(); n != 1 {
		t.Fatalf("manifest fetched %d times, want 1 (cached)", n)
	}

	// The manifest goes away: the last good one stays.
	body.Store("")
	s.android.at = time.Now().Add(-6 * time.Minute)
	if w := get("/download/android"); w.Code != http.StatusFound {
		t.Fatalf("/download/android after a failed refetch: %d", w.Code)
	}
	if n := fetches.Load(); n != 2 {
		t.Fatalf("manifest fetched %d times, want 2", n)
	}
	// A failure is retried after a minute, not 5.
	s.android.at = time.Now().Add(-61 * time.Second)
	body.Store(strings.Replace(testAndroidManifest, `"versionCode":3`, `"versionCode":4`, 1))
	if !strings.Contains(get("/").Body.String(), "(vc 4)") {
		t.Fatal("home page: the new manifest wasn't picked up a minute after a failure")
	}
}

func TestAndroidUnavailable(t *testing.T) {
	molt := httptest.NewServer(http.NotFoundHandler())
	defer molt.Close()
	for name, b := range map[string]string{
		"missing":    "",
		"not json":   "<html>",
		"incomplete": `{"versionName":"0.1.0","versionCode":3,"url":"http://insecure.example/x.apk"}`,
	} {
		var body atomic.Value
		var fetches atomic.Int32
		body.Store(b)
		s := New(Config{UpdateURL: molt.URL, AndroidManifest: androidUpstream(t, &body, &fetches)})
		r := httptest.NewRequest("GET", "/", nil)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		page := w.Body.String()
		if w.Code != 200 || strings.Contains(page, "Download the APK</a>") || !strings.Contains(page, "The APK is not available right now") ||
			!strings.Contains(page, "Get it on Google Play (beta)") {
			t.Fatalf("%s: home page %d, offers the APK or lacks Google Play", name, w.Code)
		}
		w = httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", "/download/android", nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: /download/android: %d", name, w.Code)
		}
	}
	// No manifest configured: no fetch, no APK.
	s := New(Config{UpdateURL: molt.URL})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/download/android", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no manifest: /download/android: %d", w.Code)
	}
	// /android: the short link to the Android card.
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/android", nil))
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/download#android" {
		t.Fatalf("/android: %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestAssetLinks(t *testing.T) {
	s := New(Config{})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/.well-known/assetlinks.json", nil))
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("assetlinks.json: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	var links []struct {
		Relation []string
		Target   struct {
			Namespace   string
			PackageName string   `json:"package_name"`
			Certs       []string `json:"sha256_cert_fingerprints"`
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &links); err != nil || len(links) != 1 {
		t.Fatalf("assetlinks.json: %v %s", err, w.Body)
	}
	l := links[0]
	if len(l.Relation) != 1 || l.Relation[0] != "delegate_permission/common.handle_all_urls" ||
		l.Target.Namespace != "android_app" || l.Target.PackageName != "io.saul.bellion" || len(l.Target.Certs) != 1 {
		t.Fatalf("assetlinks.json: %+v", l)
	}
	if !regexp.MustCompile(`^([0-9A-F]{2}:){31}[0-9A-F]{2}$`).MatchString(l.Target.Certs[0]) {
		t.Fatalf("assetlinks.json: bad fingerprint %q", l.Target.Certs[0])
	}
}

func TestJoinPageAndroid(t *testing.T) {
	s := New(Config{})
	join := func(ua string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/j/7k3q-x9md", nil)
		r.Header.Set("User-Agent", ua)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	w := join("Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0 Mobile Safari/537.36")
	page := w.Body.String()
	open := strings.Index(page, "Open in Bellion Share for Android")
	get := strings.Index(page, `href="/download#android">Get the Android app`)
	desktop := strings.Index(page, `href="/download">`)
	if w.Code != 200 || open < 0 || get < 0 || desktop < 0 || !(open < desktop && get < desktop) {
		t.Fatalf("Android join page: %d, the Android options missing or not above the desktop ones:\n%s", w.Code, page)
	}
	if !strings.Contains(page, `id="android-open" href="/j/7K3QX9MD"`) || !strings.Contains(w.Header().Get("Vary"), "User-Agent") {
		t.Fatal("Android join page: no https fallback link, or no Vary: User-Agent")
	}
	page = join("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0 Safari/537.36").Body.String()
	if strings.Contains(page, "Bellion Share for Android") || !strings.Contains(page, `id="retry"`) {
		t.Fatal("desktop join page: shows the Android options or lacks the desktop ones")
	}
}
