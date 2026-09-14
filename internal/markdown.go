package internal

import (
	"bytes"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	gmhtml "github.com/yuin/goldmark/renderer/html"
)

// Body formats for sequence steps.
const (
	BodyFormatMarkdown = "markdown"
	BodyFormatHTML     = "html"
	BodyFormatText     = "text"
)

// Hard wraps keep single newlines as line breaks (matching the historical
// plain-text behavior); unsafe allows hand-written HTML inside markdown.
var markdownRenderer = goldmark.New(
	goldmark.WithRendererOptions(gmhtml.WithHardWraps(), gmhtml.WithUnsafe()),
)

// MarkdownToHTML renders a markdown body to an HTML fragment.
func MarkdownToHTML(md string) (string, error) {
	var buf bytes.Buffer
	if err := markdownRenderer.Convert([]byte(strings.TrimSpace(md)), &buf); err != nil {
		return "", fmt.Errorf("rendering markdown: %w", err)
	}
	return strings.TrimSpace(buf.String()), nil
}

var (
	mdImageRe   = regexp.MustCompile(`!\[([^\]]*)\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	mdLinkRe    = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	mdHeadingRe = regexp.MustCompile(`(?m)^#{1,6}\s+`)
	mdStrongRe  = regexp.MustCompile(`(\*\*|__)(\S(?:.*?\S)?)(\*\*|__)`)
	mdEmRe      = regexp.MustCompile(`(^|[^*\w])[*_]([^*_\n]+)[*_]`)
	htmlImgRe   = regexp.MustCompile(`(?i)<img\b[^>]*\bsrc\s*=\s*["']([^"']+)["'][^>]*>`)
)

// MarkdownToPlainText produces the text/plain alternative from markdown source:
// images are dropped, links become "text (url)", and heading/emphasis markers
// are removed. Everything else is left as the author wrote it.
func MarkdownToPlainText(md string) string {
	s := strings.ReplaceAll(md, "\r\n", "\n")
	s = mdImageRe.ReplaceAllString(s, "")
	s = htmlImgRe.ReplaceAllString(s, "")
	s = mdLinkRe.ReplaceAllStringFunc(s, func(m string) string {
		parts := mdLinkRe.FindStringSubmatch(m)
		if parts[1] == parts[2] {
			return parts[1]
		}
		return parts[1] + " (" + parts[2] + ")"
	})
	s = mdHeadingRe.ReplaceAllString(s, "")
	s = mdStrongRe.ReplaceAllString(s, "$2")
	s = mdEmRe.ReplaceAllString(s, "$1$2")
	s = htmlToPlainText(s) // strips any hand-written HTML tags, unescapes entities
	s = blankRunRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

var blankRunRe = regexp.MustCompile(`\n{3,}`)

// isLocalImageRef reports whether an image reference points at a local file
// rather than a remote URL, an existing cid, or a data URI.
func isLocalImageRef(ref string) bool {
	lower := strings.ToLower(strings.TrimSpace(ref))
	for _, prefix := range []string{"http://", "https://", "cid:", "data:", "mailto:", "//"} {
		if strings.HasPrefix(lower, prefix) {
			return false
		}
	}
	return lower != ""
}

// localImageRefs returns the unique local image paths referenced by a body,
// in markdown image syntax or HTML <img> tags.
func localImageRefs(body string) []string {
	seen := map[string]bool{}
	var refs []string
	add := func(ref string) {
		ref = strings.TrimSpace(ref)
		if !isLocalImageRef(ref) || seen[ref] {
			return
		}
		seen[ref] = true
		refs = append(refs, ref)
	}
	for _, m := range mdImageRe.FindAllStringSubmatch(body, -1) {
		add(m[2])
	}
	for _, m := range htmlImgRe.FindAllStringSubmatch(body, -1) {
		add(m[1])
	}
	return refs
}

// rewriteImageRefs replaces local image references with cid: references.
func rewriteImageRefs(body string, cids map[string]string) string {
	body = mdImageRe.ReplaceAllStringFunc(body, func(m string) string {
		parts := mdImageRe.FindStringSubmatch(m)
		if cid, ok := cids[strings.TrimSpace(parts[2])]; ok {
			return strings.Replace(m, parts[2], "cid:"+cid, 1)
		}
		return m
	})
	body = htmlImgRe.ReplaceAllStringFunc(body, func(m string) string {
		parts := htmlImgRe.FindStringSubmatch(m)
		if cid, ok := cids[strings.TrimSpace(parts[1])]; ok {
			return strings.Replace(m, parts[1], "cid:"+cid, 1)
		}
		return m
	})
	return body
}

var cidUnsafeRe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// cidForPath derives a stable Content-ID from an image path.
func cidForPath(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	base = cidUnsafeRe.ReplaceAllString(base, "-")
	base = strings.Trim(base, "-.")
	if base == "" {
		base = "image"
	}
	return "img-" + strings.ToLower(base)
}
