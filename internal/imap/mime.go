package imap

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	_ "github.com/emersion/go-message/charset"
	"github.com/emersion/go-message/mail"
	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
)

const MaxText = 100 * 1024
const MaxHTML = 200 * 1024
const MaxSource = 1024 * 1024

var safeHTML = func() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowElements("p", "br", "div", "span", "b", "strong", "i", "em", "u", "ul", "ol", "li", "blockquote", "pre", "code", "h1", "h2", "h3", "table", "thead", "tbody", "tr", "th", "td")
	return p
}()

type MIMEBody struct {
	Text      string
	HTML      *string
	Truncated bool
}

func capUTF8(s string, max int) (string, bool) {
	s = strings.ToValidUTF8(s, "")
	if len(s) <= max {
		return s, false
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max], true
}

// ParseMIME decodes RFC 2047/charset/transfer encoding via go-message. It never interprets mail instructions.
func ParseMIME(raw []byte) (MIMEBody, error) {
	out := MIMEBody{}
	if len(raw) > MaxSource {
		raw = raw[:MaxSource]
		out.Truncated = true
	}
	reader, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		return out, errors.New("message MIME parsing failed")
	}
	defer reader.Close()
	plainParts, htmlParts := []string{}, []string{}
	hasPlain := false
	decoded := 0
	for i := 0; i < 1000; i++ {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, errors.New("message MIME parsing failed")
		}
		h, ok := part.Header.(*mail.InlineHeader)
		if !ok {
			continue
		}
		media, _, err := h.ContentType()
		if err != nil {
			continue
		}
		// Even text/* attachments with inline disposition and filenames are excluded from body.
		_, params, _ := h.ContentDisposition()
		if params["filename"] != "" {
			continue
		}
		if media != "text/plain" && media != "text/html" {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(part.Body, int64(MaxSource-decoded+1)))
		if err != nil {
			return out, errors.New("message MIME parsing failed")
		}
		decoded += len(b)
		if decoded > MaxSource {
			out.Truncated = true
			break
		}
		if media == "text/plain" {
			hasPlain = true
			plainParts = append(plainParts, string(b))
		} else {
			htmlParts = append(htmlParts, string(b))
		}
		if i == 999 {
			out.Truncated = true
		}
	}
	if hasPlain {
		var cut bool
		out.Text, cut = capUTF8(strings.Join(plainParts, "\n"), MaxText)
		out.Truncated = out.Truncated || cut
		return out, nil
	}
	if len(htmlParts) > 0 {
		safe := safeHTML.Sanitize(strings.Join(htmlParts, "\n"))
		text := htmlToText(safe)
		var cut bool
		out.Text, cut = capUTF8(text, MaxText)
		out.Truncated = out.Truncated || cut
		// Dropping oversized HTML preserves valid sanitized markup rather than cutting inside a tag.
		if len(safe) > MaxHTML {
			out.Truncated = true
		} else {
			out.HTML = &safe
		}
	}
	return out, nil
}
func htmlToText(s string) string {
	z := html.NewTokenizer(strings.NewReader(s))
	var b strings.Builder
	for {
		kind := z.Next()
		if kind == html.ErrorToken {
			break
		}
		switch kind {
		case html.TextToken:
			b.Write(z.Text())
		case html.StartTagToken, html.EndTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "p", "div", "br", "li", "tr", "blockquote", "pre", "h1", "h2", "h3":
				b.WriteByte('\n')
			}
		}
	}
	lines := strings.Split(b.String(), "\n")
	clean := []string{}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			clean = append(clean, line)
		}
	}
	return strings.Join(clean, "\n")
}
