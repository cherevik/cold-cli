package internal

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html"
	"mime"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// EmailParams holds everything needed to construct and send an email.
type EmailParams struct {
	FromName  string
	FromEmail string
	ToEmail   string
	CcEmails  []string
	Subject   string
	Body      string

	// Format is the Body format: markdown, html, or text (empty means text).
	Format string
	// TextBody and HTMLBody are the rendered multipart/alternative parts. When
	// empty they are derived from Body and Format at build time.
	TextBody string
	HTMLBody string
	// InlineImages are embedded as multipart/related parts addressable via cid:.
	InlineImages []InlineImage

	// For follow-ups (step 2+)
	InReplyTo  string // Message-ID of the previous step
	References string // same as InReplyTo for simple chains
	ThreadID   string // Gmail thread ID for threading
	MessageID  string // RFC Message-ID to set before sending

	// Unsubscribe
	UnsubscribeEmail   string // mailto address for List-Unsubscribe header
	UnsubscribeSubject string // subject for the mailto unsubscribe

	// Optional Date header. If zero, no Date header is added.
	Date time.Time

	// Stripped unresolved template variables (for logging)
	StrippedVars []string
}

// BuildRFCMessage constructs an RFC 2822 message.
func BuildRFCMessage(p EmailParams) string {
	var msg strings.Builder

	if !p.Date.IsZero() {
		msg.WriteString(fmt.Sprintf("Date: %s\r\n", p.Date.Format(time.RFC1123Z)))
	}

	if p.MessageID != "" {
		msg.WriteString(fmt.Sprintf("Message-ID: %s\r\n", p.MessageID))
	}

	// From header
	if p.FromName != "" {
		msg.WriteString(fmt.Sprintf("From: %s <%s>\r\n", p.FromName, p.FromEmail))
	} else {
		msg.WriteString(fmt.Sprintf("From: %s\r\n", p.FromEmail))
	}

	msg.WriteString(fmt.Sprintf("To: %s\r\n", p.ToEmail))
	if len(p.CcEmails) > 0 {
		msg.WriteString(fmt.Sprintf("Cc: %s\r\n", strings.Join(p.CcEmails, ", ")))
	}
	msg.WriteString(fmt.Sprintf("Subject: %s\r\n", encodeSubject(p.Subject)))

	// Threading headers for follow-ups
	if p.InReplyTo != "" {
		msg.WriteString(fmt.Sprintf("In-Reply-To: %s\r\n", p.InReplyTo))
		refs := p.References
		if refs == "" {
			refs = p.InReplyTo
		}
		msg.WriteString(fmt.Sprintf("References: %s\r\n", refs))
	}

	// List-Unsubscribe headers (required by Gmail/Yahoo for bulk senders)
	if p.UnsubscribeEmail != "" {
		subj := url.QueryEscape(p.UnsubscribeSubject)
		msg.WriteString(fmt.Sprintf("List-Unsubscribe: <mailto:%s?subject=%s>\r\n", p.UnsubscribeEmail, subj))
		msg.WriteString("List-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n")
	}

	msg.WriteString("MIME-Version: 1.0\r\n")

	textBody, htmlBody := RenderBodyParts(p)

	altBoundary := newMIMEBoundary()
	msg.WriteString(fmt.Sprintf("Content-Type: multipart/alternative; boundary=\"%s\"\r\n", altBoundary))
	msg.WriteString("\r\n")

	// text/plain alternative
	msg.WriteString("--" + altBoundary + "\r\n")
	msg.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	msg.WriteString("\r\n")
	msg.WriteString(textBody)
	msg.WriteString("\r\n")

	// text/html alternative, wrapped in multipart/related when images are embedded
	msg.WriteString("--" + altBoundary + "\r\n")
	if len(p.InlineImages) == 0 {
		msg.WriteString("Content-Type: text/html; charset=utf-8\r\n")
		msg.WriteString("\r\n")
		msg.WriteString(htmlBody)
		msg.WriteString("\r\n")
	} else {
		relBoundary := newMIMEBoundary()
		msg.WriteString(fmt.Sprintf("Content-Type: multipart/related; boundary=\"%s\"; type=\"text/html\"\r\n", relBoundary))
		msg.WriteString("\r\n")
		msg.WriteString("--" + relBoundary + "\r\n")
		msg.WriteString("Content-Type: text/html; charset=utf-8\r\n")
		msg.WriteString("\r\n")
		msg.WriteString(htmlBody)
		msg.WriteString("\r\n")
		for _, img := range p.InlineImages {
			filename := img.Path
			if filename == "" {
				filename = img.CID
			}
			filename = filepath.Base(filename)
			msg.WriteString("--" + relBoundary + "\r\n")
			msg.WriteString(fmt.Sprintf("Content-Type: %s; name=\"%s\"\r\n", img.ContentType, filename))
			msg.WriteString("Content-Transfer-Encoding: base64\r\n")
			msg.WriteString(fmt.Sprintf("Content-ID: <%s>\r\n", img.CID))
			msg.WriteString(fmt.Sprintf("Content-Disposition: inline; filename=\"%s\"\r\n", filename))
			msg.WriteString("\r\n")
			msg.WriteString(wrapBase64(img.Data))
			msg.WriteString("\r\n")
		}
		msg.WriteString("--" + relBoundary + "--\r\n")
	}
	msg.WriteString("--" + altBoundary + "--\r\n")

	return msg.String()
}

// RenderBodyParts returns the text/plain and text/html renderings of an email.
// Pre-rendered TextBody/HTMLBody win; otherwise they are derived from Body
// according to Format (empty Format means plain text).
func RenderBodyParts(p EmailParams) (textBody, htmlBody string) {
	textBody, htmlBody = p.TextBody, p.HTMLBody
	if textBody != "" && htmlBody != "" {
		return textBody, htmlBody
	}
	t, h := renderBody(p.Body, p.Format)
	if textBody == "" {
		textBody = t
	}
	if htmlBody == "" {
		htmlBody = h
	}
	return textBody, htmlBody
}

// renderBody converts a body in the given format to (text, html).
func renderBody(body, format string) (string, string) {
	switch format {
	case BodyFormatMarkdown:
		h, err := MarkdownToHTML(body)
		if err != nil {
			// Never fail a send over markdown rendering; fall back to plain text.
			return body, plainTextToHTML(body)
		}
		return MarkdownToPlainText(body), h
	case BodyFormatHTML:
		return htmlToPlainText(body), body
	default:
		return body, plainTextToHTML(body)
	}
}

// newMIMEBoundary returns a random multipart boundary.
func newMIMEBoundary() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("coldcli-%d", time.Now().UnixNano())
	}
	return "coldcli-" + hex.EncodeToString(b[:])
}

// wrapBase64 folds base64 text into 76-column lines as required by RFC 2045.
func wrapBase64(s string) string {
	var b strings.Builder
	for len(s) > 76 {
		b.WriteString(s[:76])
		b.WriteString("\r\n")
		s = s[76:]
	}
	b.WriteString(s)
	return b.String()
}

// referencedInlineImages returns only the images whose cid: appears in body,
// so steps without a signature do not carry unused attachments.
func referencedInlineImages(images []InlineImage, body string) []InlineImage {
	var out []InlineImage
	for _, img := range images {
		if strings.Contains(body, "cid:"+img.CID) {
			out = append(out, img)
		}
	}
	return out
}

var htmlAnchorRe = regexp.MustCompile(`(?is)<a\b[^>]*\bhref\s*=\s*["\x27]([^"\x27]+)["\x27][^>]*>(.*?)</a>`)

// htmlToPlainText produces a rough text rendering of HTML for snapshots and snippets.
func htmlToPlainText(s string) string {
	s = htmlAnchorRe.ReplaceAllStringFunc(s, func(m string) string {
		parts := htmlAnchorRe.FindStringSubmatch(m)
		href, text := strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2])
		if text == "" || text == href {
			return href
		}
		return text + " (" + href + ")"
	})
	s = regexp.MustCompile(`(?i)<br\s*/?>`).ReplaceAllString(s, "\n")
	s = regexp.MustCompile(`(?i)</(p|div|li|tr|h[1-6])>`).ReplaceAllString(s, "\n")
	s = regexp.MustCompile(`(?s)<[^>]*>`).ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// ValidateEmailParamsHeaders rejects control characters that could create
// additional RFC 5322 headers. Address syntax is validated separately by the
// caller because display names are allowed in some message sources.
func ValidateEmailParamsHeaders(p EmailParams) error {
	fields := []struct {
		name  string
		value string
	}{
		{"from name", p.FromName},
		{"from email", p.FromEmail},
		{"to email", p.ToEmail},
		{"subject", p.Subject},
		{"in-reply-to", p.InReplyTo},
		{"references", p.References},
		{"message-id", p.MessageID},
		{"unsubscribe email", p.UnsubscribeEmail},
		{"unsubscribe subject", p.UnsubscribeSubject},
	}
	for _, cc := range p.CcEmails {
		fields = append(fields, struct {
			name  string
			value string
		}{"cc email", cc})
	}
	for _, field := range fields {
		if strings.ContainsAny(field.value, "\r\n\x00") {
			return fmt.Errorf("%s contains an unsafe control character", field.name)
		}
	}
	return nil
}

// BuildRawMessage constructs an RFC 2822 message and returns it as a base64url-encoded string.
func BuildRawMessage(p EmailParams) string {
	return base64.URLEncoding.EncodeToString([]byte(BuildRFCMessage(p)))
}

// BuildEmailForSend constructs the full email for a scheduled send, applying template rendering
// and selecting the correct variant.
func BuildEmailForSend(
	seq *Sequence,
	stepNumber int,
	variantIndex int,
	lead map[string]string,
	fromEmail string,
) EmailParams {
	// Find the step
	var step *SequenceStep
	for i := range seq.Steps {
		if seq.Steps[i].Step == stepNumber {
			step = &seq.Steps[i]
			break
		}
	}
	if step == nil {
		return EmailParams{}
	}

	// Select subject and body based on variant
	subject := step.Subject
	body := step.Body
	format := step.Format
	if format == "" {
		format = BodyFormatText
	}

	if variantIndex > 0 && variantIndex <= len(step.Variants) {
		v := step.Variants[variantIndex-1]
		if v.Subject != "" {
			subject = v.Subject
		}
		if v.Body != "" {
			body = v.Body
			if v.Format != "" {
				format = v.Format
			}
		}
	}

	fields := mergeTemplateFields(lead, SenderTemplateFields(fromEmail))

	// Render templates
	fromName := RenderTemplate(seq.Defaults.FromName, fields)
	subject = RenderTemplate(subject, fields)

	// The HTML part is rendered from entity-escaped field values so lead data
	// cannot inject markup. The text part converts the template to text first
	// and then substitutes raw values, so lead data is never treated as markup.
	var htmlSource, textSource string
	if format == BodyFormatText {
		body = RenderTemplate(body, fields)
	} else {
		escaped := make(map[string]string, len(fields))
		for k, v := range fields {
			escaped[k] = html.EscapeString(v)
		}
		htmlSource = RenderTemplate(body, escaped)
		textTemplate, _ := renderBody(body, format)
		textSource = RenderTemplate(textTemplate, fields)
		body = RenderTemplate(body, fields)
	}

	// Strip any remaining unresolved {{variables}}
	var allStripped []string
	fromName, stripped := StripUnresolved(fromName)
	allStripped = append(allStripped, stripped...)
	subject, stripped = StripUnresolved(subject)
	allStripped = append(allStripped, stripped...)
	body, stripped = StripUnresolved(body)
	allStripped = append(allStripped, stripped...)
	allStripped = uniqueStrings(allStripped)

	params := EmailParams{
		FromName:     fromName,
		FromEmail:    fromEmail,
		ToEmail:      lead["email"],
		Subject:      subject,
		Body:         body,
		Format:       format,
		StrippedVars: allStripped,
	}
	if format != BodyFormatText {
		htmlSource, _ = StripUnresolved(htmlSource)
		textSource, _ = StripUnresolved(textSource)
		_, htmlPart := renderBody(htmlSource, format)
		params.TextBody = textSource
		params.HTMLBody = htmlPart
		params.InlineImages = referencedInlineImages(seq.InlineImages, htmlSource)
	}
	return params
}

func mergeTemplateFields(lead map[string]string, sender map[string]string) map[string]string {
	fields := make(map[string]string, len(lead)+len(sender))
	for key, value := range sender {
		fields[key] = value
	}
	for key, value := range lead {
		fields[key] = value
	}
	return fields
}

func uniqueStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}

	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

// plainTextToHTML converts plain text body to minimal HTML.
// Double newlines become <br><br> (paragraphs), single newlines become <br>.
// No CSS, styles, or formatting — looks like a normal hand-typed email.
func plainTextToHTML(text string) string {
	// Normalize line endings
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSpace(text)

	// Escape HTML entities
	text = strings.ReplaceAll(text, "&", "&amp;")
	text = strings.ReplaceAll(text, "<", "&lt;")
	text = strings.ReplaceAll(text, ">", "&gt;")

	// Convert newlines to <br>
	text = strings.ReplaceAll(text, "\n", "<br>")

	return "<div>" + text + "</div>"
}

// encodeSubject MIME-encodes a subject line if it contains non-ASCII characters.
func encodeSubject(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return mime.QEncoding.Encode("utf-8", s)
		}
	}
	return s
}

// PrepareFollowUp adds threading headers to an EmailParams for step 2+.
func PrepareFollowUp(p *EmailParams, parentMessageID, threadID, originalSubject string) {
	p.InReplyTo = parentMessageID
	p.References = parentMessageID
	p.ThreadID = threadID

	subject := strings.TrimSpace(p.Subject)
	if subject == "" {
		originalSubject = strings.TrimSpace(originalSubject)
		if originalSubject == "" {
			return
		}
		p.Subject = "Re: " + originalSubject
		return
	}

	// Preserve an existing reply prefix regardless of casing.
	if strings.HasPrefix(strings.ToLower(subject), "re:") {
		p.Subject = subject
		return
	}

	p.Subject = "Re: " + subject
}

// GenerateRFCMessageID creates a Message-ID scoped to the sender domain.
func GenerateRFCMessageID(fromEmail string) string {
	domain := "cold-cli.local"
	if _, after, ok := strings.Cut(strings.TrimSpace(fromEmail), "@"); ok {
		after = strings.TrimSpace(after)
		if after != "" {
			domain = after
		}
	}

	var randomBytes [12]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return fmt.Sprintf("<%d@%s>", time.Now().UnixNano(), domain)
	}
	return fmt.Sprintf("<%d.%s@%s>", time.Now().UnixNano(), hex.EncodeToString(randomBytes[:]), domain)
}
