package desktopui

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

func TestPageViewOnlyAttribute(t *testing.T) {
	off, on := Page(false), Page(true)
	if n := bytes.Count(off, viewOnlyOff); n != 1 {
		t.Fatalf("page has %d copies of %s, want exactly 1 (the gateway's replace target)", n, viewOnlyOff)
	}
	if !regexp.MustCompile(`<html\b[^>]*\bdata-view-only="false"[^>]*>`).Match(off) {
		t.Fatalf("%s is not on the <html> tag", viewOnlyOff)
	}
	i := bytes.Index(off, viewOnlyOff)
	want := append(append(bytes.Clone(off[:i]), viewOnlyOn...), off[i+len(viewOnlyOff):]...)
	if !bytes.Equal(on, want) {
		t.Fatal("Page(true) differs from Page(false) in more than the view-only attribute")
	}
	if bytes.Contains(on, viewOnlyOff) {
		t.Fatal("Page(true) still contains the view-only=false attribute")
	}
	if !bytes.Equal(off, page) {
		t.Fatal("Page(false) is not the embedded page")
	}
}

func TestPageReturnsCopy(t *testing.T) {
	p := Page(false)
	p[0] ^= 0xff
	if bytes.Equal(Page(false), p) {
		t.Fatal("Page shares its buffer with the embedded file")
	}
}

func TestPageConnectsToPath(t *testing.T) {
	s := string(Page(false))
	rel := strings.TrimPrefix(Path, "/")
	if !strings.Contains(s, `new URL("`+rel+`", location.href)`) {
		t.Fatalf("page does not resolve %q relative to its own URL", rel)
	}
}

// Comments the URL check ignores: HTML comments, block comments and
// whole-line // comments. A trailing // comment on a code line is not
// stripped, so a URL there fails the test (stricter, never looser).
var commentRE = regexp.MustCompile(`(?s)<!--.*?-->|/\*.*?\*/|(?m)^\s*//[^\n]*`)

func TestPageSelfContained(t *testing.T) {
	code := commentRE.ReplaceAllString(string(Page(false)), "")
	lower := strings.ToLower(code)
	for _, bad := range []string{"http://", "https://"} {
		if i := strings.Index(lower, bad); i >= 0 {
			t.Errorf("page has a %s URL outside comments: %q", bad, excerpt(code, i))
		}
	}
	if m := regexp.MustCompile(`(?i)<script\b[^>]*\bsrc\s*=`).FindStringIndex(code); m != nil {
		t.Errorf("page loads an external script: %q", excerpt(code, m[0]))
	}
	for _, re := range []string{`(?i)<link\b`, `(?i)@import\b`, `(?i)<(img|iframe|object|embed)\b`, `(?i)\bimport\s*\(`} {
		if m := regexp.MustCompile(re).FindStringIndex(code); m != nil {
			t.Errorf("page may load an external resource (%s): %q", re, excerpt(code, m[0]))
		}
	}
	// CSS url() loads; JavaScript's new URL(...) is fine and is outside <style>.
	for _, css := range regexp.MustCompile(`(?is)<style\b[^>]*>(.*?)</style>|\bstyle\s*=\s*"([^"]*)"`).FindAllStringSubmatch(code, -1) {
		if strings.Contains(strings.ToLower(css[1]+css[2]), "url(") {
			t.Error("page CSS uses url()")
		}
	}
}

func excerpt(s string, i int) string {
	end := min(i+60, len(s))
	return s[i:end]
}
