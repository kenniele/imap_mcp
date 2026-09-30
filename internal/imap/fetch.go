package imap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime"
	"net/mail"
	"regexp"
	"strconv"
	"strings"

	"mail-mcp/internal/domain"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/charset"
)

func fetchMetadata(c *connection, uids []imaplib.UID) ([]*imapclient.FetchMessageBuffer, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	msgs, err := c.client.Fetch(imaplib.UIDSetNum(uids...), &imaplib.FetchOptions{UID: true, Envelope: true, Flags: true, InternalDate: true, RFC822Size: true, BodyStructure: &imaplib.FetchItemBodyStructure{Extended: true}}).Collect()
	if err != nil {
		return nil, errors.New("imap operation failed")
	}
	return msgs, nil
}
func summary(a domain.Account, folder string, validity uint32, buf *imapclient.FetchMessageBuffer) domain.Message {
	m := domain.Message{Account: a.Alias, ID: fmt.Sprintf("%s:%s:%d", a.Alias, folder, buf.UID), Folder: folder, UID: uint32(buf.UID), UIDValidity: validity, From: []domain.Address{}, To: []domain.Address{}, Date: buf.InternalDate}
	if e := buf.Envelope; e != nil {
		m.Subject = e.Subject
		m.MessageID = messageID(e.MessageID)
		m.From = addresses(e.From)
		m.To = addresses(e.To)
		m.CC = addresses(e.Cc)
		if !e.Date.IsZero() {
			m.Date = e.Date
		}
	}
	for _, f := range buf.Flags {
		if f == imaplib.FlagSeen {
			m.Seen = true
		}
	}
	_, atts := bodyParts(buf.BodyStructure, m.ID)
	m.HasAttachments = len(atts) > 0
	return m
}
func messageID(s string) string {
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "<") {
		return s
	}
	return "<" + s + ">"
}
func addresses(in []imaplib.Address) []domain.Address {
	out := []domain.Address{}
	for _, a := range in {
		if email := a.Addr(); email != "" {
			out = append(out, domain.Address{Name: a.Name, Email: email})
		}
	}
	return out
}

type textPart struct {
	path  []int
	media string
	size  uint32
}

func bodyParts(bs imaplib.BodyStructure, id string) ([]textPart, []domain.Attachment) {
	parts := []textPart{}
	atts := []domain.Attachment{}
	if bs == nil {
		return parts, atts
	}
	decoder := mime.WordDecoder{CharsetReader: charset.Reader}
	bs.Walk(func(path []int, bs imaplib.BodyStructure) bool {
		disposition := bs.Disposition()
		attached := disposition != nil && strings.EqualFold(disposition.Value, "attachment")
		if multi, ok := bs.(*imaplib.BodyStructureMultiPart); ok {
			_ = multi
			if attached {
				partID := partPath(path)
				atts = append(atts, domain.Attachment{ContentType: bs.MediaType(), ID: id + ":" + partID})
				return false
			}
			return true
		}
		single, ok := bs.(*imaplib.BodyStructureSinglePart)
		if !ok {
			return false
		}
		filename := single.Filename()
		if decoded, err := decoder.DecodeHeader(filename); err == nil {
			filename = decoded
		}
		media := single.MediaType()
		if attached || filename != "" || (media != "text/plain" && media != "text/html") {
			atts = append(atts, domain.Attachment{Filename: filename, ContentType: media, Size: single.Size, ID: id + ":" + partPath(path)})
			return false
		}
		parts = append(parts, textPart{append([]int(nil), path...), media, single.Size})
		return false
	})
	return parts, atts
}
func partPath(path []int) string {
	p := []string{}
	for _, v := range path {
		p = append(p, strconv.Itoa(v))
	}
	return strings.Join(p, ".")
}
func (p *Provider) Get(ctx context.Context, a domain.Account, folder string, uid, validity uint32, attachments bool) (out domain.Content, err error) {
	err = p.with(ctx, a, "get", func(c *connection) error {
		selected, e := selectFolder(c, folder, validity)
		if e != nil {
			return e
		}
		out, e = getContent(c, a, folder, uid, selected.UIDValidity, attachments)
		return e
	})
	return out, err
}
func getContent(c *connection, a domain.Account, folder string, uid, validity uint32, attachments bool) (domain.Content, error) {
	bufs, err := fetchMetadata(c, []imaplib.UID{imaplib.UID(uid)})
	if err != nil {
		return domain.Content{}, err
	}
	if len(bufs) != 1 {
		return domain.Content{}, errors.New("message not found")
	}
	buf := bufs[0]
	out := domain.Content{Message: summary(a, folder, validity, buf), Attachments: []domain.Attachment{}}
	parts, atts := bodyParts(buf.BodyStructure, out.ID)
	if attachments {
		out.Attachments = atts
	}
	header, err := fetchSection(c, uid, &imaplib.FetchItemBodySection{Specifier: imaplib.PartSpecifierHeader, Peek: true, Partial: &imaplib.SectionPartial{Size: 64 * 1024}})
	if err != nil {
		return out, err
	}
	out.InReplyTo, out.References = parseReferences(header)
	hasPlain := false
	for _, part := range parts {
		if part.media == "text/plain" {
			hasPlain = true
		}
	}
	total := 0
	selectedParts := 0
	for _, part := range parts {
		if hasPlain && part.media != "text/plain" {
			continue
		}
		selectedParts++
		if selectedParts > 16 || total >= MaxSource {
			out.Truncated = true
			break
		}
		path := part.path
		// A non-multipart root's body uses BODY[TEXT], not BODY[1], for rev1 interoperability.
		spec := imaplib.PartSpecifierNone
		if _, single := buf.BodyStructure.(*imaplib.BodyStructureSinglePart); single {
			path = nil
			spec = imaplib.PartSpecifierText
		}
		mimeSpec := imaplib.PartSpecifierMIME
		if path == nil {
			mimeSpec = imaplib.PartSpecifierHeader
		}
		h, e := fetchSection(c, uid, &imaplib.FetchItemBodySection{Part: path, Specifier: mimeSpec, Peek: true, Partial: &imaplib.SectionPartial{Size: 16 * 1024}})
		if e != nil {
			return out, e
		}
		maxEncoded := min(512*1024, (MaxSource-total-len(h)-4)/4*4)
		if maxEncoded <= 0 {
			out.Truncated = true
			break
		}
		b, e := fetchSection(c, uid, &imaplib.FetchItemBodySection{Part: path, Specifier: spec, Peek: true, Partial: &imaplib.SectionPartial{Size: int64(maxEncoded)}})
		if e != nil {
			return out, e
		}
		cut := int(part.size) > len(b)
		out.Truncated = out.Truncated || cut
		// Avoid a partial trailing quoted-printable escape. Base64 cap is a multiple of four.
		if cut && bytes.Contains(bytes.ToLower(h), []byte("quoted-printable")) {
			if n := bytes.LastIndexByte(b, '='); n >= 0 && len(b)-n <= 2 {
				b = b[:n]
			}
		}
		raw := append(append(bytes.TrimRight(h, "\r\n"), []byte("\r\n\r\n")...), b...)
		parsed, e := ParseMIME(raw)
		if e != nil {
			return out, e
		}
		total += len(raw)
		if parsed.Text != "" {
			if out.Text != "" {
				out.Text += "\n"
			}
			out.Text += parsed.Text
		}
		if parsed.HTML != nil {
			if out.HTML == nil {
				v := *parsed.HTML
				out.HTML = &v
			} else {
				v := *out.HTML + "\n" + *parsed.HTML
				out.HTML = &v
			}
		}
		out.Truncated = out.Truncated || parsed.Truncated
	}
	var cut bool
	out.Text, cut = capUTF8(out.Text, MaxText)
	out.Truncated = out.Truncated || cut
	if out.HTML != nil && len(*out.HTML) > MaxHTML {
		out.HTML = nil
		out.Truncated = true
	}
	return out, nil
}
func fetchSection(c *connection, uid uint32, section *imaplib.FetchItemBodySection) ([]byte, error) {
	msgs, err := c.client.Fetch(imaplib.UIDSetNum(imaplib.UID(uid)), &imaplib.FetchOptions{UID: true, BodySection: []*imaplib.FetchItemBodySection{section}}).Collect()
	if err != nil {
		return nil, errors.New("imap operation failed")
	}
	if len(msgs) != 1 {
		return nil, errors.New("message not found")
	}
	return msgs[0].FindBodySection(section), nil
}

var referencePattern = regexp.MustCompile(`<[^<>\s]+>`)

func parseReferences(raw []byte) ([]string, []string) {
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, nil
	}
	return referencePattern.FindAllString(message.Header.Get("In-Reply-To"), -1), referencePattern.FindAllString(message.Header.Get("References"), -1)
}

func buffersForUID(buffers []*imapclient.FetchMessageBuffer, uid imaplib.UID) *imapclient.FetchMessageBuffer {
	for _, b := range buffers {
		if b.UID == uid {
			return b
		}
	}
	return nil
}
func fetchPreview(c *connection, uid uint32, buf *imapclient.FetchMessageBuffer) (string, error) {
	if buf == nil {
		return "", nil
	}
	parts, _ := bodyParts(buf.BodyStructure, "")
	if len(parts) == 0 {
		return "", nil
	}
	chosen := parts[0]
	for _, part := range parts {
		if part.media == "text/plain" {
			chosen = part
			break
		}
	}
	path := chosen.path
	spec := imaplib.PartSpecifierNone
	mimeSpec := imaplib.PartSpecifierMIME
	if _, single := buf.BodyStructure.(*imaplib.BodyStructureSinglePart); single {
		path = nil
		spec = imaplib.PartSpecifierText
		mimeSpec = imaplib.PartSpecifierHeader
	}
	h, e := fetchSection(c, uid, &imaplib.FetchItemBodySection{Part: path, Specifier: mimeSpec, Peek: true, Partial: &imaplib.SectionPartial{Size: 16 * 1024}})
	if e != nil {
		return "", e
	}
	b, e := fetchSection(c, uid, &imaplib.FetchItemBodySection{Part: path, Specifier: spec, Peek: true, Partial: &imaplib.SectionPartial{Size: 4096}})
	if e != nil {
		return "", e
	}
	if chosen.size > 4096 && bytes.Contains(bytes.ToLower(h), []byte("quoted-printable")) {
		if n := bytes.LastIndexByte(b, '='); n >= 0 && len(b)-n <= 2 {
			b = b[:n]
		}
	}
	raw := append(append(bytes.TrimRight(h, "\r\n"), []byte("\r\n\r\n")...), b...)
	body, e := ParseMIME(raw)
	if e != nil {
		return "", nil
	} // A malformed preview must not hide otherwise searchable headers.
	preview, _ := capUTF8(body.Text, 300)
	return preview, nil
}
