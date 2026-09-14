package internal

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildRFCMessage_HTMLWithInlineImage(t *testing.T) {
	msg := BuildRFCMessage(EmailParams{
		FromName:  "Alex",
		FromEmail: "sender@example.com",
		ToEmail:   "john@acme.com",
		Subject:   "Hello",
		Body:      `<p>Hi <a href="https://example.com">link</a></p><img src="cid:logo">`,
		Format:    BodyFormatHTML,
		InlineImages: []InlineImage{{
			CID: "logo", Path: "logo.png", ContentType: "image/png",
			Data: base64.StdEncoding.EncodeToString([]byte("pngbytes")),
		}},
	})

	for _, want := range []string{
		`Content-Type: multipart/alternative; boundary="`,
		"Content-Type: text/plain; charset=utf-8\r\n\r\nHi link (https://example.com)",
		`Content-Type: multipart/related; boundary="`,
		`type="text/html"`,
		"Content-Type: text/html; charset=utf-8",
		`<a href="https://example.com">link</a>`,
		"Content-ID: <logo>",
		`Content-Disposition: inline; filename="logo.png"`,
		"Content-Transfer-Encoding: base64",
		base64.StdEncoding.EncodeToString([]byte("pngbytes")),
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected message to contain %q\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "&lt;a") {
		t.Error("HTML body must not be escaped")
	}
	if !strings.HasSuffix(msg, "--\r\n") {
		t.Error("multipart message must end with closing boundary")
	}
}

func TestBuildRFCMessage_NoImagesHasNoRelatedPart(t *testing.T) {
	msg := BuildRFCMessage(EmailParams{
		FromEmail: "s@example.com", ToEmail: "t@example.com", Subject: "x",
		Body: "<b>bold</b>", Format: BodyFormatHTML,
	})
	if strings.Contains(msg, "multipart/related") {
		t.Error("no images: expected no multipart/related wrapper")
	}
	if !strings.Contains(msg, "\r\n\r\n<b>bold</b>") {
		t.Errorf("expected raw HTML body, got:\n%s", msg)
	}
	if !strings.Contains(msg, "text/plain; charset=utf-8\r\n\r\nbold") {
		t.Errorf("expected plain-text alternative, got:\n%s", msg)
	}
}

func TestMarkdownToHTML_HardWrapsLinksAndRawHTML(t *testing.T) {
	html, err := MarkdownToHTML("Hi **there**,\nline two\n\n[Site](https://x.io) and <a href=\"https://y.io\">raw</a>\n\n- one\n- two")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<p>Hi <strong>there</strong>,<br>\nline two</p>",
		`<a href="https://x.io">Site</a>`,
		`<a href="https://y.io">raw</a>`,
		"<ul>", "<li>one</li>",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("expected %q in:\n%s", want, html)
		}
	}
}

func TestMarkdownToPlainText(t *testing.T) {
	got := MarkdownToPlainText("Hi **there**,\n\nSee [the doc](https://x.io/doc) and [https://y.io](https://y.io).\n\n![badge](cid:img-badge)\n\nBye")
	want := "Hi there,\n\nSee the doc (https://x.io/doc) and https://y.io.\n\nBye"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestBuildEmailForSend_MarkdownDefaultEscapesFieldsAndFiltersImages(t *testing.T) {
	seq := &Sequence{
		Steps: []SequenceStep{
			{Step: 1, Subject: "Hi {{first_name}}", Body: "Hello {{company}}\n\n![b](cid:logo)", Format: BodyFormatMarkdown},
			{Step: 2, Body: "follow up {{first_name}}", Format: BodyFormatMarkdown},
			{Step: 3, Body: "plain {{first_name}} <not html>", Format: BodyFormatText},
		},
		InlineImages: []InlineImage{{CID: "logo", ContentType: "image/png", Data: "AA=="}},
	}
	lead := map[string]string{"email": "j@acme.com", "first_name": "J", "company": "A&B <Co>"}

	p1 := BuildEmailForSend(seq, 1, 0, lead, "s@example.com")
	if p1.Format != BodyFormatMarkdown {
		t.Errorf("expected markdown format, got %q", p1.Format)
	}
	if !strings.Contains(p1.HTMLBody, "Hello A&amp;B &lt;Co&gt;") {
		t.Errorf("HTML part should escape lead fields, got %q", p1.HTMLBody)
	}
	if !strings.Contains(p1.TextBody, "Hello A&B <Co>") || strings.Contains(p1.TextBody, "![") {
		t.Errorf("text part should use raw values without image syntax, got %q", p1.TextBody)
	}
	if !strings.Contains(p1.HTMLBody, `<img src="cid:logo"`) {
		t.Errorf("HTML part should reference the cid image, got %q", p1.HTMLBody)
	}
	if len(p1.InlineImages) != 1 {
		t.Errorf("step 1 references cid:logo, expected 1 image, got %d", len(p1.InlineImages))
	}
	p2 := BuildEmailForSend(seq, 2, 0, lead, "s@example.com")
	if len(p2.InlineImages) != 0 {
		t.Errorf("step 2 has no cid reference, expected 0 images, got %d", len(p2.InlineImages))
	}
	p3 := BuildEmailForSend(seq, 3, 0, lead, "s@example.com")
	if p3.Format != BodyFormatText || p3.Body != "plain J <not html>" {
		t.Errorf("step 3 should stay plain text unescaped, got %+v", p3)
	}
	if got := BuildRFCMessage(p3); !strings.Contains(got, "&lt;not html&gt;") {
		t.Error("plain step must still be escaped in the HTML part")
	}
}

func TestParseSequence_AutoEmbedsMarkdownImages(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sig"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sig", "badge.jpg"), []byte("fakejpg"), 0o600); err != nil {
		t.Fatal(err)
	}
	yml := "name: t\nsteps:\n  - step: 1\n    subject: s\n    body: |\n      Hi\n\n      ![Badge](sig/badge.jpg)\n  - step: 2\n    body: |\n      Bump <img src=\"sig/badge.jpg\"> and ![remote](https://x.io/a.png)\n"
	path := filepath.Join(dir, "seq.yml")
	if err := os.WriteFile(path, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	seq, err := ParseSequence(path)
	if err != nil {
		t.Fatalf("ParseSequence: %v", err)
	}
	if seq.Steps[0].Format != BodyFormatMarkdown {
		t.Errorf("default format should be markdown, got %q", seq.Steps[0].Format)
	}
	if len(seq.InlineImages) != 1 {
		t.Fatalf("expected one auto-embedded image, got %+v", seq.InlineImages)
	}
	img := seq.InlineImages[0]
	if img.CID != "img-badge" || img.ContentType != "image/jpeg" || img.Data != base64.StdEncoding.EncodeToString([]byte("fakejpg")) {
		t.Errorf("unexpected image: %+v", img)
	}
	if !strings.Contains(seq.Steps[0].Body, "![Badge](cid:img-badge)") {
		t.Errorf("markdown ref not rewritten: %q", seq.Steps[0].Body)
	}
	if !strings.Contains(seq.Steps[1].Body, `<img src="cid:img-badge">`) || !strings.Contains(seq.Steps[1].Body, "https://x.io/a.png") {
		t.Errorf("html ref not rewritten or remote ref altered: %q", seq.Steps[1].Body)
	}

	out, err := seq.SelfContainedYAML()
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParseSequenceFromBytes(out)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if err := again.LoadInlineImages("/nonexistent"); err != nil {
		t.Errorf("self-contained YAML should not need files on disk: %v", err)
	}
	if again.InlineImages[0].Data != img.Data || !strings.Contains(again.Steps[0].Body, "cid:img-badge") {
		t.Error("data or cid rewrite lost in round trip")
	}
}

func TestParseSequence_FormatValidation(t *testing.T) {
	if _, err := ParseSequenceFromBytes([]byte("steps:\n  - step: 1\n    subject: s\n    body: b\n    format: rtf\n")); err == nil {
		t.Error("expected error for unknown format")
	}
	seq, err := ParseSequenceFromBytes([]byte("steps:\n  - step: 1\n    subject: s\n    body: b\n    html: true\n    variants:\n      - body: v\n"))
	if err != nil {
		t.Fatal(err)
	}
	if seq.Steps[0].Format != BodyFormatHTML || seq.Steps[0].Variants[0].Format != BodyFormatHTML {
		t.Errorf("html alias / variant inheritance failed: %+v", seq.Steps[0])
	}
}

func TestParseSequence_InlineImageErrors(t *testing.T) {
	seq := &Sequence{InlineImages: []InlineImage{{CID: "a", Path: "missing.png"}}}
	if err := seq.LoadInlineImages(t.TempDir()); err == nil {
		t.Error("expected error for missing file")
	}
	seq = &Sequence{InlineImages: []InlineImage{{CID: "a", Data: "AA=="}, {CID: "a", Data: "AA=="}}}
	if err := seq.LoadInlineImages("."); err == nil {
		t.Error("expected duplicate cid error")
	}
}

func TestHTMLToPlainText(t *testing.T) {
	got := htmlToPlainText(`<p>Hi &amp; bye</p><div>line<br>two</div><img src="cid:x">`)
	want := "Hi & bye\nline\ntwo"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}
