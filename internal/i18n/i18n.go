// Package i18n translates the app's user-visible text. The catalogs are
// go-i18n JSON files (the format Fyne's lang package uses) in translations/,
// keyed by the English source text, so an untranslated string shows in
// English. The language comes from BLITE_LANG, else the user's choice
// (View > Language, kept in the config dir), else the system's locale.
package i18n

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"

	"github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/text/language"
)

//go:embed translations/*.json
var files embed.FS

// Language is one the app is translated to.
type Language struct {
	Tag  string // file name and BCP 47 tag, e.g. "zh-Hans"
	Name string // in the language itself, e.g. "日本語"
	// RTL: written right to left. Fyne lays text out left to right only,
	// so these aren't picked from the system locale (choose them by hand).
	RTL bool
}

// Languages lists the translations, English first.
var Languages = []Language{
	{Tag: "en", Name: "English"},
	{Tag: "zh-Hans", Name: "简体中文"},
	{Tag: "zh-Hant", Name: "繁體中文"},
	{Tag: "hi", Name: "हिन्दी"},
	{Tag: "es", Name: "Español"},
	{Tag: "fr", Name: "Français"},
	{Tag: "ar", Name: "العربية", RTL: true},
	{Tag: "bn", Name: "বাংলা"},
	{Tag: "pt-BR", Name: "Português (Brasil)"},
	{Tag: "ru", Name: "Русский"},
	{Tag: "ur", Name: "اردو", RTL: true},
	{Tag: "id", Name: "Bahasa Indonesia"},
	{Tag: "de", Name: "Deutsch"},
	{Tag: "ja", Name: "日本語"},
	{Tag: "ko", Name: "한국어"},
	{Tag: "tr", Name: "Türkçe"},
	{Tag: "vi", Name: "Tiếng Việt"},
	{Tag: "it", Name: "Italiano"},
	{Tag: "pl", Name: "Polski"},
	{Tag: "uk", Name: "Українська"},
	{Tag: "fa", Name: "فارسی", RTL: true},
	{Tag: "th", Name: "ไทย"},
	{Tag: "nl", Name: "Nederlands"},
	{Tag: "ta", Name: "தமிழ்"},
	{Tag: "te", Name: "తెలుగు"},
	{Tag: "mr", Name: "मराठी"},
	{Tag: "fil", Name: "Filipino"},
	{Tag: "sw", Name: "Kiswahili"},
}

var (
	once    sync.Once
	bundle  *i18n.Bundle
	english *i18n.Localizer
	active  atomic.Pointer[state]
	// patterns match English messages with placeholders, for Msg.
	patterns []pattern
	// sources are the English catalog's keys.
	sources map[string]bool
)

type state struct {
	tag string
	loc *i18n.Localizer
}

func setup() {
	once.Do(func() {
		bundle = i18n.NewBundle(language.English)
		bundle.RegisterUnmarshalFunc("json", json.Unmarshal)
		names, _ := fs.Glob(files, "translations/*.json")
		for _, n := range names {
			b, _ := files.ReadFile(n)
			if _, err := bundle.ParseMessageFileBytes(b, path.Base(n)); err != nil {
				panic("i18n: " + n + ": " + err.Error()) // caught by the tests
			}
		}
		english = i18n.NewLocalizer(bundle, "en")
		loadPatterns()
		apply(initial())
	})
}

// initial is the language at start: BLITE_LANG, the user's choice, the
// system's.
func initial() string {
	if t := match([]string{os.Getenv("BLITE_LANG")}, true); t != "" {
		return t
	}
	if t := Preference(); t != "" {
		return t
	}
	return System()
}

// System is the language the system's locale picks ("en" if none of ours).
func System() string {
	if t := match(systemLocales(), false); t != "" {
		return t
	}
	return "en"
}

func apply(tag string) {
	active.Store(&state{tag: tag, loc: i18n.NewLocalizer(bundle, tag)})
}

// Current is the language in use, e.g. "de".
func Current() string {
	setup()
	return active.Load().tag
}

// Lookup returns the language with tag, or false.
func Lookup(tag string) (Language, bool) {
	for _, l := range Languages {
		if l.Tag == tag {
			return l, true
		}
	}
	return Language{}, false
}

// match picks our language for the first of locales we have, e.g.
// "de_DE.UTF-8" -> "de", "zh-TW" -> "zh-Hant"; "" for none. rtl allows
// the right-to-left languages.
func match(locales []string, rtl bool) string {
	for _, loc := range locales {
		loc = strings.TrimSpace(loc)
		if i := strings.IndexAny(loc, ".@"); i >= 0 { // de_DE.UTF-8, sr_RS@latin
			loc = loc[:i]
		}
		loc = strings.ReplaceAll(loc, "_", "-")
		if loc == "" || loc == "C" || loc == "POSIX" {
			continue
		}
		t, err := language.Parse(loc)
		if err != nil {
			continue
		}
		base, _ := t.Base()
		tag := base.String()
		switch tag {
		case "zh":
			tag = "zh-Hans"
			if s, _ := t.Script(); s.String() == "Hant" {
				tag = "zh-Hant"
			}
		case "pt":
			tag = "pt-BR" // one Portuguese, Brazil's, also for Portugal
		case "tl":
			tag = "fil"
		}
		if l, ok := Lookup(tag); ok && (rtl || !l.RTL) {
			return tag
		}
	}
	return ""
}

// prefFile keeps the user's choice of language.
func prefFile() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "blite", "language.json"), nil
}

type pref struct {
	Lang string `json:"lang"` // "" = the system's
}

// prefCache keeps the file's choice: the menu asks on every refresh.
var prefCache struct {
	sync.Mutex
	file, lang string
}

// Preference is the language the user chose, "" for the system's.
func Preference() string {
	f, err := prefFile()
	if err != nil {
		return ""
	}
	prefCache.Lock()
	defer prefCache.Unlock()
	if prefCache.file == f {
		return prefCache.lang
	}
	prefCache.file, prefCache.lang = f, ""
	b, err := os.ReadFile(f)
	if err != nil {
		return ""
	}
	var p pref
	if json.Unmarshal(b, &p) != nil {
		return ""
	}
	if _, ok := Lookup(p.Lang); ok {
		prefCache.lang = p.Lang
	}
	return prefCache.lang
}

// SetPreference keeps tag as the user's language ("" = the system's) and
// switches to it.
func SetPreference(tag string) error {
	if _, ok := Lookup(tag); tag != "" && !ok {
		return errors.New("unknown language " + tag)
	}
	setup()
	if tag == "" {
		apply(System())
	} else {
		apply(tag)
	}
	f, err := prefFile()
	if err != nil {
		return err
	}
	prefCache.Lock()
	prefCache.file, prefCache.lang = f, tag
	prefCache.Unlock()
	if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(pref{Lang: tag})
	return os.WriteFile(f, b, 0o600)
}

// D is template data: i18n.L("Hi {{.Name}}", i18n.D{"Name": n}).
type D = map[string]any

// L translates the English text s; data fills its {{.Placeholders}}.
func L(s string, data ...map[string]any) string {
	setup()
	var d map[string]any
	if len(data) > 0 {
		d = data[0]
	}
	return localize(active.Load().loc, &i18n.LocalizeConfig{MessageID: s, TemplateData: d}, s, d)
}

// N translates s in the plural form for count, which also fills
// {{.Count}}.
func N(s string, count int, data ...map[string]any) string {
	setup()
	d := map[string]any{"Count": count}
	if len(data) > 0 {
		for k, v := range data[0] {
			d[k] = v
		}
	}
	return localize(active.Load().loc, &i18n.LocalizeConfig{MessageID: s, PluralCount: count, TemplateData: d}, s, d)
}

// localize falls back to English, then to s itself.
func localize(loc *i18n.Localizer, c *i18n.LocalizeConfig, s string, d map[string]any) string {
	if out, err := loc.Localize(c); err == nil {
		return out
	}
	if out, err := english.Localize(c); err == nil {
		return out
	}
	return fill(s, d)
}

func fill(s string, d map[string]any) string {
	if !strings.Contains(s, "{{") {
		return s
	}
	t, err := template.New("").Option("missingkey=zero").Parse(s)
	if err != nil {
		return s
	}
	var b strings.Builder
	if t.Execute(&b, d) != nil {
		return s
	}
	return b.String()
}

// pattern matches an English message with placeholders.
type pattern struct {
	key   string
	re    *regexp.Regexp
	names []string
	fixed int // length of the literal text: longer matches first
}

var placeholder = regexp.MustCompile(`\{\{\s*\.(\w+)\s*\}\}`)

func loadPatterns() {
	b, err := files.ReadFile("translations/en.json")
	if err != nil {
		return
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return
	}
	sources = map[string]bool{}
	for key, v := range m {
		sources[key] = true
		if _, ok := v.(string); !ok || !placeholder.MatchString(key) {
			continue // plurals aren't messages from elsewhere
		}
		p := pattern{key: key}
		var re strings.Builder
		re.WriteString(`(?s)^`)
		last := 0
		for _, loc := range placeholder.FindAllStringSubmatchIndex(key, -1) {
			re.WriteString(regexp.QuoteMeta(key[last:loc[0]]))
			p.fixed += loc[0] - last
			re.WriteString(`(.+?)`)
			p.names = append(p.names, key[loc[2]:loc[3]])
			last = loc[1]
		}
		re.WriteString(regexp.QuoteMeta(key[last:]))
		p.fixed += len(key) - last
		re.WriteString(`$`)
		p.re = regexp.MustCompile(re.String())
		patterns = append(patterns, p)
	}
	sort.Slice(patterns, func(i, j int) bool {
		if patterns[i].fixed != patterns[j].fixed {
			return patterns[i].fixed > patterns[j].fixed
		}
		return patterns[i].key < patterns[j].key
	})
}

// Msg translates a message made elsewhere in English (an error, a note
// from the session): the catalog has it as is, or as a template whose
// placeholders it fills ("{{.Err}}" parts are translated too). Others stay
// as they are.
func Msg(s string) string {
	setup()
	return msg(s, 0)
}

func msg(s string, depth int) string {
	if s == "" || depth > 3 {
		return s
	}
	if sources[s] && !placeholder.MatchString(s) {
		return L(s)
	}
	for _, p := range patterns {
		m := p.re.FindStringSubmatch(s)
		if m == nil {
			continue
		}
		d := map[string]any{}
		for i, n := range p.names {
			v := m[i+1]
			if n == "Err" {
				v = msg(v, depth+1)
			}
			d[n] = v
		}
		return L(p.key, d)
	}
	return s
}

// Err translates err's message (Msg); nil stays nil.
func Err(err error) error {
	if err == nil {
		return nil
	}
	if t := Msg(err.Error()); t != err.Error() {
		return errors.New(t)
	}
	return err
}

// Keys lists the English catalog's keys, sorted (for the tests).
func Keys() []string {
	setup()
	var k []string
	for s := range sources {
		k = append(k, s)
	}
	slices.Sort(k)
	return k
}
