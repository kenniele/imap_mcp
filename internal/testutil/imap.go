package testutil

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log"
	"math/big"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mail-mcp/internal/domain"
	"mail-mcp/internal/secrets"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

type Fixture struct {
	Cipher      *secrets.Cipher
	Roots       *x509.CertPool
	Host        string
	Port        int
	Server      *imapmemserver.Server
	InvalidRead atomic.Bool
	Sessions    atomic.Int32
	Delays      sync.Map
	stop        chan struct{}
}
type auditingSession struct {
	imapserver.Session
	fixture *Fixture
}

func (s *auditingSession) Select(folder string, o *imaplib.SelectOptions) (*imaplib.SelectData, error) {
	if !o.ReadOnly {
		s.fixture.InvalidRead.Store(true)
	}
	return s.Session.Select(folder, o)
}
func (s *auditingSession) Search(kind imapserver.NumKind, c *imaplib.SearchCriteria, o *imaplib.SearchOptions) (*imaplib.SearchData, error) {
	if kind != imapserver.NumKindUID {
		s.fixture.InvalidRead.Store(true)
	}
	return s.Session.Search(kind, c, o)
}
func (s *auditingSession) Fetch(w *imapserver.FetchWriter, n imaplib.NumSet, o *imaplib.FetchOptions) error {
	if _, ok := n.(imaplib.UIDSet); !ok {
		s.fixture.InvalidRead.Store(true)
	}
	for _, body := range o.BodySection {
		if !body.Peek {
			s.fixture.InvalidRead.Store(true)
		}
	}
	return s.Session.Fetch(w, n, o)
}
func NewIMAP(t *testing.T) *Fixture {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test IMAP"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	cipher, err := secrets.New(bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	f := &Fixture{Cipher: cipher, Roots: roots, Host: "127.0.0.1", Server: imapmemserver.New(), stop: make(chan struct{})}
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(tcp.Addr().String())
	f.Port, _ = strconv.Atoi(port)
	listener := tls.NewListener(tcp, &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12})
	server := imapserver.New(&imapserver.Options{NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		f.Sessions.Add(1)
		return &auditingSession{Session: f.Server.NewSession(), fixture: f}, nil, nil
	}, Logger: log.New(io.Discard, "", 0)})
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	t.Cleanup(func() {
		close(f.stop)
		_ = server.Close()
		<-done
		if f.InvalidRead.Load() {
			t.Error("non-read-only or sequence-number command observed")
		}
	})
	return f
}

type literal struct {
	*bytes.Reader
	size int64
}

func (r literal) Size() int64 { return r.size }
func (f *Fixture) Account(t *testing.T, alias string, messages []string) (domain.Account, *imapmemserver.User) {
	t.Helper()
	user := imapmemserver.NewUser(alias, "app-password")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	if err := user.Create("Sent", nil); err != nil {
		t.Fatal(err)
	}
	f.Server.AddUser(user)
	for _, raw := range messages {
		f.Append(t, user, raw)
	}
	secret, err := f.Cipher.Encrypt([]byte("app-password"), "account:"+alias)
	if err != nil {
		t.Fatal(err)
	}
	return domain.Account{ID: alias, Alias: alias, Email: alias + "@example.com", Provider: "custom", IMAPHost: f.Host, IMAPPort: f.Port, IMAPTLS: true, Username: alias, Secret: secret, Enabled: true}, user
}
func (f *Fixture) Append(t *testing.T, user *imapmemserver.User, raw string) {
	t.Helper()
	b := []byte(raw)
	if _, err := user.Append("INBOX", literal{bytes.NewReader(b), int64(len(b))}, &imaplib.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
}

const Plain = "From: Teacher <teacher@example.com>\r\nTo: Student <student@example.com>\r\nSubject: =?UTF-8?B?0JLQmtCg?=\r\nDate: Wed, 30 Sep 2026 08:42:00 +0300\r\nMessage-ID: <root@example.com>\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n0JTQvtCx0YDRi9C5INC00LXQvdGMINCa0LjRgNC40LvQuyE="
const HTML = "From: Teacher <teacher@example.com>\r\nTo: Student <student@example.com>\r\nSubject: HTML only\r\nDate: Wed, 30 Sep 2026 09:42:00 +0300\r\nMessage-ID: <reply@example.com>\r\nIn-Reply-To: <root@example.com>\r\nReferences: <root@example.com>\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>Привет &amp; hello</p><script>steal()</script><img src=\"https://tracker.test/pixel\"><a href=\"https://evil.test\">link</a>"
const Multipart = "From: Teacher <teacher@example.com>\r\nTo: Student <student@example.com>\r\nSubject: Attachments\r\nDate: Wed, 30 Sep 2026 10:42:00 +0300\r\nMessage-ID: <attachment@example.com>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=mixed\r\n\r\n--mixed\r\nContent-Type: multipart/alternative; boundary=alt\r\n\r\n--alt\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nhello=20world\r\n--alt\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>html alternative</p>\r\n--alt--\r\n--mixed\r\nContent-Type: application/pdf; name=document.pdf\r\nContent-Disposition: attachment; filename=document.pdf\r\nContent-Transfer-Encoding: base64\r\n\r\nYmluYXJ5LXNlY3JldA==\r\n--mixed--\r\n"

func (s *auditingSession) Login(username, password string) error {
	if delay, ok := s.fixture.Delays.Load(username); ok {
		timer := time.NewTimer(delay.(time.Duration))
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-s.fixture.stop:
			return imapserver.ErrAuthFailed
		}
	}
	return s.Session.Login(username, password)
}
