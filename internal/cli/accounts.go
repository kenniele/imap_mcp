package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"os"
	"strconv"
	"strings"
	"time"

	"mail-mcp/internal/app"
	"mail-mcp/internal/domain"
	"mail-mcp/internal/secrets"

	"golang.org/x/term"
)

type Store interface {
	app.AccountStore
	Add(context.Context, domain.Account) error
	Disable(context.Context, string) error
	Delete(context.Context, string) error
}

func Run(ctx context.Context, args []string, store Store, provider app.MailProvider, cipher *secrets.Cipher) error {
	if len(args) == 0 {
		return errors.New("usage: mail-mcp account add|list|test|disable|delete [alias]")
	}
	switch args[0] {
	case "add":
		if len(args) != 1 {
			return errors.New("usage: mail-mcp account add")
		}
		return add(ctx, store, cipher)
	case "list":
		if len(args) != 1 {
			return errors.New("usage: mail-mcp account list")
		}
		accounts, err := store.List(ctx)
		if err != nil {
			return err
		}
		type row struct {
			ID, Alias, Email, Provider string
			Enabled                    bool
		}
		rows := []row{}
		for _, a := range accounts {
			rows = append(rows, row{a.ID, a.Alias, a.Email, a.Provider, a.Enabled})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	case "test", "disable", "delete":
		if len(args) != 2 {
			return errors.New("account alias is required")
		}
		if args[0] == "disable" {
			return store.Disable(ctx, args[1])
		}
		if args[0] == "delete" {
			return store.Delete(ctx, args[1])
		}
		all, err := store.List(ctx)
		if err != nil {
			return err
		}
		selected, err := app.SelectAccounts(all, []string{args[1]})
		if err != nil {
			return err
		}
		testCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := provider.Test(testCtx, selected[0]); err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, "IMAP TLS authentication: ok")
		return nil
	default:
		return errors.New("unknown account command")
	}
}
func prompt(reader *bufio.Reader, label string) (string, error) {
	fmt.Fprint(os.Stderr, label+": ")
	value, err := reader.ReadString('\n')
	if err != nil {
		return "", io.ErrUnexpectedEOF
	}
	return strings.TrimSpace(value), nil
}
func add(ctx context.Context, store Store, cipher *secrets.Cipher) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("account add requires an interactive terminal; passwords are never accepted as CLI arguments")
	}
	reader := bufio.NewReader(os.Stdin)
	alias, err := prompt(reader, "Alias")
	if err != nil {
		return err
	}
	if !app.ValidAlias(alias) {
		return errors.New("alias must match [a-z][a-z0-9_-]{0,63}")
	}
	email, err := prompt(reader, "Email")
	if err != nil {
		return err
	}
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Address != email {
		return errors.New("invalid email address")
	}
	provider, err := prompt(reader, "Provider [mailru/yandex/gmail/custom; default mailru]")
	if err != nil {
		return err
	}
	if provider == "" {
		provider = "mailru"
	}
	preset, known := domain.Presets[provider]
	username := email
	if provider == "custom" {
		preset.TLS = true
		preset.IMAPHost, err = prompt(reader, "IMAP TLS hostname")
		if err != nil {
			return err
		}
		if preset.IMAPHost == "" || strings.ContainsAny(preset.IMAPHost, "/: \t\r\n") {
			return errors.New("invalid hostname")
		}
		value, e := prompt(reader, "IMAP TLS port [993]")
		if e != nil {
			return e
		}
		if value == "" {
			value = "993"
		}
		preset.IMAPPort, e = strconv.Atoi(value)
		if e != nil || preset.IMAPPort < 1 || preset.IMAPPort > 65535 {
			return errors.New("invalid port")
		}
		username, err = prompt(reader, "Username [default email]")
		if err != nil {
			return err
		}
		if username == "" {
			username = email
		}
	} else if !known {
		return errors.New("unknown provider")
	}
	password, err := readPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return errors.New("password read failed")
	}
	defer clear(password)
	if len(password) == 0 {
		return errors.New("empty application password")
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	id := fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
	encrypted, err := cipher.Encrypt(password, "account:"+id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := store.Add(ctx, domain.Account{ID: id, Alias: alias, Email: email, Provider: provider, IMAPHost: preset.IMAPHost, IMAPPort: preset.IMAPPort, IMAPTLS: true, Username: username, Secret: encrypted, Enabled: true}); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "Account added:", alias)
	return nil
}

// Disable terminal echo before showing the prompt: pasted input can arrive immediately.
func readPassword(fd int) ([]byte, error) {
	previous, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	defer term.Restore(fd, previous)
	fmt.Fprint(os.Stderr, "Application password (not the main account password): ")
	return term.ReadPassword(fd)
}
