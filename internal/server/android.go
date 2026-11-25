package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The Android app (Bellion Share for Android, a viewer) isn't on the update
// server: aws/android-publish.sh (blite-ops) puts the signed APK and a small
// JSON manifest next to the site in S3. The download page reads the manifest
// (Config.AndroidManifest) and links the APK.

// androidPackage is the Android app's package name.
const androidPackage = "io.saul.bellion"

// androidCertSHA256 is the SHA-256 of the certificate the Android app is
// signed with (apksigner verify --print-certs), for assetlinks.json. It
// doesn't change between versions: a new key would be a new app to Android.
const androidCertSHA256 = "28:57:9D:C3:73:7F:64:50:E5:4D:3E:6B:58:C2:FD:B9:C2:0E:C8:A5:E8:E1:05:CF:0E:E9:F9:14:A7:19:FC:4D"

// androidRelease is the manifest android-publish.sh writes.
type androidRelease struct {
	VersionName string    `json:"versionName"`
	VersionCode int       `json:"versionCode"`
	Build       int       `json:"build"` // the desktop build it was made with
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	CertSHA256  string    `json:"certSha256"`
	URL         string    `json:"url"`
	Released    time.Time `json:"released"`
	MinAndroid  string    `json:"minAndroid"`
}

// androidInfo caches the Android manifest for the download page, like
// releases: refetched at most every 5 minutes; a failed fetch keeps the last
// good one (and is retried after a minute while there is none).
type androidInfo struct {
	url  string
	mu   sync.Mutex
	at   time.Time
	ok   bool
	last *androidRelease
}

func (a *androidInfo) get(ctx context.Context) *androidRelease {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.at.IsZero() {
		ttl := time.Minute
		if a.ok {
			ttl = 5 * time.Minute
		}
		if time.Since(a.at) < ttl {
			return a.last
		}
	}
	// One visitor leaving mustn't count as a failed fetch for everyone.
	rel, err := fetchAndroid(context.WithoutCancel(ctx), a.url)
	a.at, a.ok = time.Now(), err == nil
	if err == nil {
		a.last = rel
	}
	return a.last
}

func fetchAndroid(ctx context.Context, url string) (*androidRelease, error) {
	c := http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, res.Status)
	}
	var rel androidRelease
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&rel); err != nil {
		return nil, err
	}
	if rel.VersionName == "" || rel.VersionCode <= 0 || !strings.HasPrefix(rel.URL, "https://") {
		return nil, fmt.Errorf("%s: incomplete manifest", url)
	}
	return &rel, nil
}

// androidDL is the Android release as the download page shows it.
type androidDL struct {
	VersionName string
	VersionCode int
	Size        string
	Released    string
	MinAndroid  string
	SHA256      string
	CertSHA256  string
}

func newAndroidDL(x *androidRelease) *androidDL {
	minAndroid := x.MinAndroid
	if minAndroid == "" {
		minAndroid = "10"
	}
	return &androidDL{x.VersionName, x.VersionCode, fmt.Sprintf("%.0f MB", float64(x.Size)/1e6),
		x.Released.Format("January 2, 2006"), minAndroid, strings.ToLower(x.SHA256), strings.ToUpper(x.CertSHA256)}
}

// assetLinksJSON lets Android verify that the app may open bellion.app links
// (App Links): /.well-known/assetlinks.json.
var assetLinksJSON = func() string {
	b, err := json.Marshal([]any{map[string]any{
		"relation": []string{"delegate_permission/common.handle_all_urls"},
		"target": map[string]any{
			"namespace":                "android_app",
			"package_name":             androidPackage,
			"sha256_cert_fingerprints": []string{androidCertSHA256},
		},
	}})
	if err != nil {
		panic(err)
	}
	return string(b) + "\n"
}()
