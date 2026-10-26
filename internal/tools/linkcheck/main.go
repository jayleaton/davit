// Command linkcheck checks the relative links of every Markdown file in the
// repository: `[text](path)`, `[text](path#anchor)`, `![alt](path)`,
// reference definitions (`[id]: path`) and `<img src>` / `<a href>`.
//
//   - The target file or directory must exist. A leading `/` is the repo root.
//   - An `#anchor` into a .md file (the same file or another one) must match a
//     heading's GitHub slug or an explicit `<a id="...">` / `<a name="...">`.
//   - URLs with a scheme (http:, https:, mailto:, ...) are not checked, and
//     neither is anything inside fenced code blocks or inline code.
//
// It walks the directories given as arguments (default "."), skipping
// dot-directories (.git, .build, ...), node_modules and results (git-ignored
// test output).
//
// Usage: go run ./internal/tools/linkcheck [DIR...]. It prints
// `file:line: broken link X (reason)` for each broken link and exits 1 if
// there is any.
package main

import (
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// skipDirs are directory paths (relative to a walked root, slash-separated)
// that are never checked. Dot-directories are skipped as well.
var skipDirs = map[string]bool{
	"node_modules": true,
	"results":      true,
}

func main() {
	roots := os.Args[1:]
	if len(roots) == 0 {
		roots = []string{"."}
	}
	c := &checker{anchors: map[string]map[string]bool{}}
	files := 0
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				if rel != "." && (strings.HasPrefix(d.Name(), ".") || skipDirs[rel] || d.Name() == "node_modules") {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(path, ".md") {
				files++
				c.checkFile(root, path)
			}
			return nil
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "linkcheck:", err)
			os.Exit(2)
		}
	}
	if c.broken > 0 {
		fmt.Printf("linkcheck: %d broken link(s) in %d Markdown files\n", c.broken, files)
		os.Exit(1)
	}
	fmt.Printf("linkcheck: %d links ok in %d Markdown files\n", c.links, files)
}

type checker struct {
	anchors map[string]map[string]bool // file path -> its anchors
	links   int
	broken  int
}

var (
	// inlineLink matches the target of [text](target) and ![alt](target),
	// with an optional title.
	inlineLink = regexp.MustCompile(`\]\(\s*(<[^>]*>|[^()\s]+)(?:\s+(?:"[^"]*"|'[^']*'))?\s*\)`)
	// refDef matches a reference definition: [id]: target.
	refDef = regexp.MustCompile(`^\s{0,3}\[[^\]]+\]:\s*(<[^>]*>|\S+)`)
	// htmlLink matches <img src="..."> and <a href="...">.
	htmlLink = regexp.MustCompile(`<(?:img|a)\b[^>]*?\s(?:src|href)\s*=\s*["']([^"']*)["']`)
	// htmlAnchor matches an explicit <a id="..."> or <a name="...">.
	htmlAnchor = regexp.MustCompile(`<a\b[^>]*?\s(?:id|name)\s*=\s*["']([^"']*)["']`)
	heading    = regexp.MustCompile(`^\s{0,3}(#{1,6})\s+(.*?)\s*$`)
	scheme     = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)
	mdLink     = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	htmlTag    = regexp.MustCompile(`<[^>]+>`)
)

// line is one line of a Markdown file outside fenced code, with inline code
// removed.
type line struct {
	n    int
	text string
}

// proseLines returns the lines of src outside fenced code blocks, with inline
// code spans blanked out.
func proseLines(src string) []line {
	var out []line
	fence := "" // the opening fence while inside a fenced block
	for i, l := range strings.Split(src, "\n") {
		t := strings.TrimLeft(l, " ")
		indent := len(l) - len(t)
		if fence != "" {
			if indent <= 3 && strings.HasPrefix(t, fence) && strings.Trim(t, fence[:1]+" \t") == "" {
				fence = ""
			}
			continue
		}
		if indent <= 3 && (strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")) {
			n := len(t) - len(strings.TrimLeft(t, t[:1]))
			fence = strings.Repeat(t[:1], n)
			continue
		}
		out = append(out, line{n: i + 1, text: stripInlineCode(l)})
	}
	return out
}

// stripInlineCode removes code spans: a run of N backticks up to the next run
// of exactly N backticks. An unmatched run is kept as text.
func stripInlineCode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '`' {
			b.WriteByte(s[i])
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] == '`' {
			j++
		}
		run := s[i:j]
		end := -1
		for k := j; k < len(s); {
			if s[k] != '`' {
				k++
				continue
			}
			m := k
			for m < len(s) && s[m] == '`' {
				m++
			}
			if m-k == len(run) {
				end = m
				break
			}
			k = m
		}
		if end < 0 {
			b.WriteString(run)
			i = j
			continue
		}
		b.WriteString(" ")
		i = end
	}
	return b.String()
}

// Slug is GitHub's heading anchor: lowercase; keep letters, digits, spaces,
// hyphens and underscores; spaces become hyphens. Inline markup is removed
// first (link targets, HTML tags, backticks).
func Slug(heading string) string {
	h := mdLink.ReplaceAllString(heading, "$1")
	h = htmlTag.ReplaceAllString(h, "")
	var b strings.Builder
	for _, r := range strings.ToLower(h) {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	return b.String()
}

// anchorsOf returns the anchors of the Markdown file at path (nil if it
// cannot be read): heading slugs, with -1, -2, ... for repeats, plus
// explicit <a id/name> anchors.
func (c *checker) anchorsOf(path string) map[string]bool {
	if a, ok := c.anchors[path]; ok {
		return a
	}
	src, err := os.ReadFile(path)
	if err != nil {
		c.anchors[path] = nil
		return nil
	}
	a := map[string]bool{}
	seen := map[string]int{}
	for _, l := range proseLinesKeepCode(string(src)) {
		for _, m := range htmlAnchor.FindAllStringSubmatch(l.text, -1) {
			a[m[1]] = true
		}
		m := heading.FindStringSubmatch(l.text)
		if m == nil {
			continue
		}
		text := strings.TrimRight(m[2], "#")
		text = strings.TrimSpace(text)
		s := Slug(text)
		if n, dup := seen[s]; dup {
			seen[s] = n + 1
			a[fmt.Sprintf("%s-%d", s, n+1)] = true
		} else {
			seen[s] = 0
			a[s] = true
		}
	}
	c.anchors[path] = a
	return a
}

// proseLinesKeepCode is proseLines with inline code kept, because a heading's
// slug includes the text of its code spans.
func proseLinesKeepCode(src string) []line {
	all := strings.Split(src, "\n")
	var out []line
	for _, l := range proseLines(src) {
		out = append(out, line{n: l.n, text: all[l.n-1]})
	}
	return out
}

func (c *checker) checkFile(root, path string) {
	src, err := os.ReadFile(path)
	if err != nil {
		c.report(path, 0, "", err.Error())
		return
	}
	for _, l := range proseLines(string(src)) {
		var targets []string
		for _, m := range inlineLink.FindAllStringSubmatch(l.text, -1) {
			targets = append(targets, m[1])
		}
		if m := refDef.FindStringSubmatch(l.text); m != nil {
			targets = append(targets, m[1])
		}
		for _, m := range htmlLink.FindAllStringSubmatch(l.text, -1) {
			targets = append(targets, m[1])
		}
		for _, t := range targets {
			c.checkLink(root, path, l.n, strings.Trim(t, "<>"))
		}
	}
}

func (c *checker) checkLink(root, from string, n int, target string) {
	if target == "" || scheme.MatchString(target) {
		return
	}
	c.links++
	p, anchor, _ := strings.Cut(target, "#")
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	if dec, err := url.PathUnescape(p); err == nil {
		p = dec
	}
	if dec, err := url.PathUnescape(anchor); err == nil {
		anchor = dec
	}
	dest := from
	if p != "" {
		if strings.HasPrefix(p, "/") {
			dest = filepath.Join(root, filepath.FromSlash(p))
		} else {
			dest = filepath.Join(filepath.Dir(from), filepath.FromSlash(p))
		}
		st, err := os.Stat(dest)
		if err != nil {
			c.report(from, n, target, "no such file or directory")
			return
		}
		if st.IsDir() {
			if anchor != "" {
				c.report(from, n, target, "anchor on a directory")
			}
			return
		}
	}
	if anchor == "" || !strings.HasSuffix(dest, ".md") {
		return
	}
	if !c.anchorsOf(dest)[anchor] {
		c.report(from, n, target, "no heading or <a id> \""+anchor+"\" in "+filepath.ToSlash(dest))
	}
}

func (c *checker) report(file string, n int, target, reason string) {
	c.broken++
	fmt.Printf("%s:%d: broken link %s (%s)\n", filepath.ToSlash(file), n, target, reason)
}
