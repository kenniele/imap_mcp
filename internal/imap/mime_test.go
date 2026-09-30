package imap

import (
	"strings"
	"testing"
	"unicode/utf8"

	"mail-mcp/internal/testutil"
)

func TestMIME(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
		html            bool
	}{{"Russian base64", testutil.Plain, "Добрый день Кирилл!", false}, {"HTML only", testutil.HTML, "Привет & hello", true}, {"multipart", testutil.Multipart, "hello world", false}, {"KOI8-R", "Content-Type: text/plain; charset=koi8-r\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n=F0=D2=C9=D7=C5=D4", "Привет", false}} {
		t.Run(tc.name, func(t *testing.T) {
			b, e := ParseMIME([]byte(tc.raw))
			if e != nil {
				t.Fatal(e)
			}
			if !strings.Contains(b.Text, tc.want) {
				t.Fatalf("text=%q", b.Text)
			}
			if (b.HTML != nil) != tc.html {
				t.Fatalf("html=%v", b.HTML)
			}
			if b.HTML != nil {
				for _, unsafe := range []string{"script", "img", "src=", "href=", "tracker", "steal"} {
					if strings.Contains(*b.HTML, unsafe) {
						t.Fatal("unsafe HTML retained")
					}
				}
			}
			if strings.Contains(b.Text, "binary-secret") {
				t.Fatal("attachment leaked")
			}
		})
	}
}
func TestMIMEBudgets(t *testing.T) {
	raw := "Content-Type: text/plain; charset=utf-8\r\n\r\n" + strings.Repeat("я", MaxText)
	out, e := ParseMIME([]byte(raw))
	if e != nil {
		t.Fatal(e)
	}
	if !out.Truncated || len(out.Text) > MaxText || !utf8.ValidString(out.Text) {
		t.Fatal("UTF-8 text budget not enforced")
	}
	raw = "Content-Type: text/html; charset=utf-8\r\n\r\n<p>" + strings.Repeat("a", MaxHTML+1) + "</p>"
	out, e = ParseMIME([]byte(raw))
	if e != nil {
		t.Fatal(e)
	}
	if !out.Truncated || out.HTML != nil {
		t.Fatal("HTML budget not enforced")
	}
}
