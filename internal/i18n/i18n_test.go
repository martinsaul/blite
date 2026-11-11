package i18n

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/nicksnyder/go-i18n/v2/i18n"
)

// catalog reads translations/<tag>.json: key -> string, or plural forms.
func catalog(t *testing.T, tag string) map[string]any {
	t.Helper()
	b, err := files.ReadFile("translations/" + tag + ".json")
	if err != nil {
		t.Fatalf("%s: %v", tag, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s.json: %v", tag, err)
	}
	return m
}

func names(s string) []string {
	var out []string
	for _, m := range placeholder.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// testLangs are the languages to check: all, or I18N_LANGS=de,fr.
func testLangs() []Language {
	only := os.Getenv("I18N_LANGS")
	if only == "" {
		return Languages
	}
	var out []Language
	for _, l := range Languages {
		if slices.Contains(strings.Split(only, ","), l.Tag) {
			out = append(out, l)
		}
	}
	return out
}

var pluralForms = []string{"zero", "one", "two", "few", "many", "other"}

// TestCatalogs: every language has every English key, nothing else, and the
// same placeholders; plurals have the forms their language needs.
func TestCatalogs(t *testing.T) {
	setup()
	en := catalog(t, "en")
	glob, _ := fs.Glob(files, "translations/*.json")
	if len(glob) != len(Languages) {
		t.Errorf("%d catalogs for %d languages", len(glob), len(Languages))
	}
	for _, l := range testLangs() {
		if l.Tag == "en" {
			continue
		}
		c := catalog(t, l.Tag)
		loc := i18n.NewLocalizer(bundle, l.Tag)
		for k, ev := range en {
			v, ok := c[k]
			if !ok {
				t.Errorf("%s: missing %q", l.Tag, k)
				continue
			}
			switch ev := ev.(type) {
			case string:
				s, ok := v.(string)
				if !ok || strings.TrimSpace(s) == "" {
					t.Errorf("%s: %q: want a non-empty string", l.Tag, k)
					continue
				}
				if !slices.Equal(names(s), names(ev)) {
					t.Errorf("%s: %q: placeholders %v, want %v", l.Tag, k, names(s), names(ev))
				}
			case map[string]any:
				forms, ok := v.(map[string]any)
				if !ok {
					t.Errorf("%s: %q: want plural forms", l.Tag, k)
					continue
				}
				want := names(ev["other"].(string))
				for f, fv := range forms {
					s, _ := fv.(string)
					if !slices.Contains(pluralForms, f) || strings.TrimSpace(s) == "" {
						t.Errorf("%s: %q: bad form %q", l.Tag, k, f)
						continue
					}
					got := names(s)
					// A form may leave the number out ("one person"), not add any.
					for _, n := range got {
						if !slices.Contains(want, n) {
							t.Errorf("%s: %q (%s): placeholder %s isn't in English", l.Tag, k, f, n)
						}
					}
					if f == "other" && !slices.Equal(got, want) {
						t.Errorf("%s: %q (other): placeholders %v, want %v", l.Tag, k, got, want)
					}
				}
				for _, n := range []int{0, 1, 2, 3, 4, 5, 6, 10, 11, 12, 21, 22, 25, 100, 101, 102, 111, 1000, 1000000} {
					if _, err := loc.Localize(&i18n.LocalizeConfig{MessageID: k, PluralCount: n, TemplateData: D{"Count": n}}); err != nil {
						t.Errorf("%s: %q for %d: %v", l.Tag, k, n, err)
					}
				}
			}
		}
		for k := range c {
			if _, ok := en[k]; !ok {
				t.Errorf("%s: %q isn't in en.json", l.Tag, k)
			}
		}
	}
}

// used finds the i18n.L and i18n.N calls in the app's Go files: key -> plural.
func used(t *testing.T) map[string]bool {
	t.Helper()
	keys := map[string]bool{}
	root := "../.."
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "third_party" || strings.HasPrefix(d.Name(), ".")) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "i18n" || sel.Sel.Name != "L" && sel.Sel.Name != "N" {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Errorf("%s: i18n.%s needs a string literal", p, sel.Sel.Name)
				return true
			}
			s, _ := strconv.Unquote(lit.Value)
			keys[s] = sel.Sel.Name == "N"
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

// TestKeysInCode: what the code translates is in en.json, plurals as
// plurals; en.json's other keys are messages made elsewhere, so their
// text must still be in the code.
func TestKeysInCode(t *testing.T) {
	en := catalog(t, "en")
	keys := used(t)
	for k, plural := range keys {
		v, ok := en[k]
		switch {
		case !ok:
			t.Errorf("en.json lacks %q", k)
		case plural:
			if _, ok := v.(map[string]any); !ok {
				t.Errorf("%q: i18n.N, but en.json has no plural forms", k)
			}
		default:
			if s, ok := v.(string); !ok || s != k {
				t.Errorf("%q: en.json should map it to itself", k)
			}
		}
	}
	var src strings.Builder
	filepath.WalkDir("../..", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			b, _ := os.ReadFile(p)
			src.Write(b)
		}
		return nil
	})
	code := src.String()
	for k := range en {
		if _, ok := keys[k]; ok {
			continue
		}
		for _, part := range placeholder.Split(k, -1) {
			if part = strings.TrimSpace(part); part != "" && !strings.Contains(code, part) {
				t.Errorf("en.json: %q: %q isn't in the code any more", k, part)
			}
		}
	}
}

func TestMatch(t *testing.T) {
	for _, c := range []struct {
		in   []string
		rtl  bool
		want string
	}{
		{[]string{"de_DE.UTF-8"}, false, "de"},
		{[]string{"de-AT"}, false, "de"},
		{[]string{"zh_CN.UTF-8"}, false, "zh-Hans"},
		{[]string{"zh-TW"}, false, "zh-Hant"},
		{[]string{"zh-HK"}, false, "zh-Hant"},
		{[]string{"zh-Hant"}, false, "zh-Hant"},
		{[]string{"zh"}, false, "zh-Hans"},
		{[]string{"pt_PT"}, false, "pt-BR"},
		{[]string{"pt-BR"}, false, "pt-BR"},
		{[]string{"tl_PH"}, false, "fil"},
		{[]string{"fil-PH"}, false, "fil"},
		{[]string{"in"}, false, "id"},
		{[]string{"C"}, false, ""},
		{[]string{"POSIX"}, false, ""},
		{[]string{""}, false, ""},
		{[]string{"sv_SE", "fr_FR"}, false, "fr"},
		{[]string{"sv_SE"}, false, ""},
		{[]string{"ar_EG.UTF-8", "en_US"}, false, "en"}, // right to left: not by itself
		{[]string{"ar_EG"}, true, "ar"},
		{[]string{"sr_RS@latin"}, false, ""},
		{[]string{"en_GB"}, false, "en"},
		{[]string{"es-419"}, false, "es"},
		{[]string{"ja_JP.eucJP"}, false, "ja"},
	} {
		if got := match(c.in, c.rtl); got != c.want {
			t.Errorf("match(%q, %v) = %q, want %q", c.in, c.rtl, got, c.want)
		}
	}
}

func TestMsg(t *testing.T) {
	setup()
	defer apply(Current())
	apply("en")
	if got := Msg("could not start sharing: not sharing"); got != "could not start sharing: not sharing" {
		t.Error(got)
	}
	apply("de")
	for in, want := range map[string]string{
		"wrong password for that room":                                         L("wrong password for that room"),
		"could not start sharing: not sharing":                                 L("could not start sharing: {{.Err}}", D{"Err": L("not sharing")}),
		"Bob removed you from the room.":                                       L("{{.Name}} removed you from the room.", D{"Name": "Bob"}),
		"Bob removed you from the room and banned you from rejoining.":         L("{{.Name}} removed you from the room and banned you from rejoining.", D{"Name": "Bob"}),
		"Your computer can't encode 4K at 60 fps (41 fps); sharing at 30 fps.": L("Your computer can't encode {{.Resolution}} at 60 fps ({{.FPS}} fps); sharing at 30 fps.", D{"Resolution": "4K", "FPS": "41"}),
		"dial wss://x: connection refused":                                     "dial wss://x: connection refused",
		"":                                                                     "",
	} {
		if got := Msg(in); got != want {
			t.Errorf("Msg(%q) = %q, want %q", in, got, want)
		}
		if in != "" && in != "dial wss://x: connection refused" && Msg(in) == in {
			t.Errorf("Msg(%q) wasn't translated", in)
		}
	}
	if got := N("You relay for {{.Count}} people", 1); got == N("You relay for {{.Count}} people", 2) {
		t.Errorf("one and other read the same: %q", got)
	}
}

// TestFallback: a key the catalog lacks shows as is, filled in.
func TestFallback(t *testing.T) {
	if got := L("not a key {{.X}}", D{"X": 1}); got != "not a key 1" {
		t.Error(got)
	}
	if got := L("Mute"); got == "" {
		t.Error("empty")
	}
}

func TestPreference(t *testing.T) {
	setup()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("AppData", dir)
	defer apply(Current())
	if Preference() != "" {
		t.Fatal("a preference in a fresh config dir")
	}
	if err := SetPreference("ja"); err != nil {
		t.Fatal(err)
	}
	if Current() != "ja" || Preference() != "ja" {
		t.Errorf("current %s, preference %s", Current(), Preference())
	}
	if L("Mute") == "Mute" {
		t.Error("not switched to Japanese")
	}
	if err := SetPreference("xx"); err == nil {
		t.Error("an unknown language was accepted")
	}
	if err := SetPreference(""); err != nil || Preference() != "" || Current() != System() {
		t.Errorf("automatic: %v, preference %q, current %s", err, Preference(), Current())
	}
}
