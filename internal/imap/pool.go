package imap

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"mime"
	"net"
	"strconv"
	"sync"
	"time"

	"mail-mcp/internal/domain"
	"mail-mcp/internal/secrets"

	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/charset"
)

type connection struct {
	client      *imapclient.Client
	conn        net.Conn
	used        time.Time
	fingerprint [32]byte
}

func (c *connection) close() { _ = c.conn.Close(); _ = c.client.Close() }

type bucket struct {
	slots chan struct{}
	idle  []*connection
	all   map[*connection]bool
}
type Pool struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	cipher  *secrets.Cipher
	roots   *x509.CertPool
	closed  bool
	done    chan struct{}
	wg      sync.WaitGroup
}

func NewPool(cipher *secrets.Cipher, roots *x509.CertPool) *Pool {
	p := &Pool{buckets: map[string]*bucket{}, cipher: cipher, roots: roots, done: make(chan struct{})}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-p.done:
				return
			case <-t.C:
				p.reap()
			}
		}
	}()
	return p
}
func fingerprint(a domain.Account) [32]byte {
	return sha256.Sum256(append([]byte(a.IMAPHost+":"+strconv.Itoa(a.IMAPPort)+":"+a.Username), a.Secret...))
}
func (p *Pool) Acquire(ctx context.Context, a domain.Account) (*connection, error) {
	if !a.IMAPTLS || a.IMAPHost == "" || a.IMAPPort < 1 || a.IMAPPort > 65535 {
		return nil, errors.New("TLS IMAP account configuration required")
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errors.New("IMAP pool closed")
	}
	b := p.buckets[a.ID]
	if b == nil {
		b = &bucket{slots: make(chan struct{}, 2), all: map[*connection]bool{}}
		p.buckets[a.ID] = b
	}
	p.mu.Unlock()
	select {
	case b.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.done:
		return nil, errors.New("IMAP pool closed")
	}
	fp := fingerprint(a)
	p.mu.Lock()
	for len(b.idle) > 0 {
		n := len(b.idle) - 1
		c := b.idle[n]
		b.idle = b.idle[:n]
		closed := false
		select {
		case <-c.client.Closed():
			closed = true
		default:
		}
		if !p.closed && !closed && c.fingerprint == fp && time.Since(c.used) < 5*time.Minute {
			p.mu.Unlock()
			slog.DebugContext(ctx, "imap connection acquired", append(traceAttrs(ctx), "connection_reused", true)...)
			return c, nil
		}
		delete(b.all, c)
		c.close()
	}
	closed := p.closed
	p.mu.Unlock()
	if closed {
		<-b.slots
		return nil, errors.New("IMAP pool closed")
	}
	c, err := p.dial(ctx, a)
	if err != nil {
		<-b.slots
		return nil, err
	}
	c.fingerprint = fp
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		c.close()
		<-b.slots
		return nil, errors.New("IMAP pool closed")
	}
	b.all[c] = true
	p.mu.Unlock()
	slog.DebugContext(ctx, "imap connection acquired", append(traceAttrs(ctx), "connection_reused", false)...)
	return c, nil
}
func (p *Pool) dial(ctx context.Context, a domain.Account) (*connection, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	password, err := step(ctx, "decrypt_credentials", func() ([]byte, error) { return p.cipher.Decrypt(a.Secret, "account:"+a.ID) })
	if err != nil {
		return nil, err
	}
	defer clear(password)
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: a.IMAPHost, RootCAs: p.roots}}
	conn, err := step(ctx, "tls_connect", func() (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp", net.JoinHostPort(a.IMAPHost, strconv.Itoa(a.IMAPPort)))
	}, "port", a.IMAPPort)
	if err != nil {
		return nil, err
	}
	c := &connection{conn: conn, client: imapclient.New(conn, &imapclient.Options{WordDecoder: &mime.WordDecoder{CharsetReader: charset.Reader}})}
	stop := watch(ctx, c)
	defer stop()
	if err = commandStep(ctx, "greeting", c.client.WaitGreeting); err != nil {
		c.close()
		return nil, err
	}
	if err = commandStep(ctx, "login", func() error { return c.client.Login(a.Username, string(password)).Wait() }); err != nil {
		c.close()
		return nil, err
	}
	return c, nil
}

// watch interrupts SDK operations even when the SDK overrides net.Conn deadlines.
func watch(ctx context.Context, c *connection) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = c.conn.Close(); close(done) })
	return func() {
		if !stop() {
			<-done
		}
	}
}
func (p *Pool) Release(a domain.Account, c *connection)    { p.finish(a, c, false) }
func (p *Pool) Invalidate(a domain.Account, c *connection) { p.finish(a, c, true) }
func (p *Pool) finish(a domain.Account, c *connection, bad bool) {
	p.mu.Lock()
	b := p.buckets[a.ID]
	if bad || p.closed {
		delete(b.all, c)
		c.close()
	} else {
		c.used = time.Now()
		b.idle = append(b.idle, c)
	}
	p.mu.Unlock()
	<-b.slots
}
func (p *Pool) reap() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, b := range p.buckets {
		keep := b.idle[:0]
		for _, c := range b.idle {
			if time.Since(c.used) >= 5*time.Minute {
				delete(b.all, c)
				c.close()
			} else {
				keep = append(keep, c)
			}
		}
		b.idle = keep
	}
}
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.done)
	for _, b := range p.buckets {
		for c := range b.all {
			c.close()
		}
		b.idle = nil
	}
	p.mu.Unlock()
	p.wg.Wait()
}
