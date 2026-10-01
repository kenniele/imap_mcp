package imap

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"mail-mcp/internal/domain"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

func TestFetchMetadataBodyStructureParameters(t *testing.T) {
	for _, tc := range []struct {
		name, params string
		wantError    bool
	}{
		{name: "standard", params: `("BOUNDARY" "mixed")`},
		{name: "mailru_nil_value", params: `("BOUNDARY" NIL "NAME" "kept")`},
		{name: "empty_string_value", params: `("BOUNDARY" "")`},
		{name: "absent_parameters", params: `NIL`},
		{name: "incomplete_pair", params: `("BOUNDARY")`, wantError: true},
		{name: "nil_name", params: `(NIL "mixed")`, wantError: true},
		{name: "unsupported_atom", params: `("BOUNDARY" UNKNOWN)`, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clientConn, serverConn := net.Pipe()
			_ = clientConn.SetDeadline(time.Now().Add(3 * time.Second))
			_ = serverConn.SetDeadline(time.Now().Add(3 * time.Second))
			client := imapclient.New(clientConn, nil)
			t.Cleanup(func() { _ = client.Close(); _ = serverConn.Close() })
			done := make(chan error, 1)
			go func() {
				defer serverConn.Close()
				_, err := fmt.Fprint(serverConn, "* PREAUTH [CAPABILITY IMAP4rev1] ready\r\n")
				if err != nil {
					done <- err
					return
				}
				line, err := bufio.NewReader(serverConn).ReadString('\n')
				if err != nil {
					done <- err
					return
				}
				fields := strings.Fields(line)
				if len(fields) < 4 || fields[1] != "UID" || fields[2] != "FETCH" || !strings.Contains(line, "BODYSTRUCTURE") {
					done <- fmt.Errorf("expected UID FETCH BODYSTRUCTURE")
					return
				}
				// Synthetic multipart extension with real attachment metadata.
				body := `(("TEXT" "PLAIN" ("CHARSET" "utf-8") NIL NIL "7BIT" 12 1 NIL NIL NIL NIL)("APPLICATION" "PDF" ("NAME" "document.pdf") NIL NIL "BASE64" 24 NIL ("ATTACHMENT" ("FILENAME" "document.pdf")) NIL NIL) "MIXED" ` + tc.params + ` NIL NIL NIL)`
				_, err = fmt.Fprintf(serverConn, "* 1 FETCH (UID 42 BODYSTRUCTURE %s)\r\n%s OK fetched\r\n", body, fields[0])
				done <- err
			}()
			if err := client.WaitGreeting(); err != nil {
				t.Fatalf("greeting: %v", err)
			}
			buffers, err := fetchMetadata(context.Background(), &connection{client: client, conn: clientConn}, []imaplib.UID{42})
			if tc.wantError {
				if err == nil || err.Error() != "imap operation failed" {
					t.Fatalf("expected controlled parser failure, got %v", err)
				}
				return
			}
			if err != nil || len(buffers) != 1 {
				t.Fatalf("metadata count=%d error=%v", len(buffers), err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			message := summary(domain.Account{Alias: "mailru"}, "INBOX", 1, buffers[0])
			parts, attachments := bodyParts(buffers[0].BodyStructure, message.ID)
			if !message.HasAttachments || len(parts) != 1 || len(attachments) != 1 || attachments[0].Filename != "document.pdf" || attachments[0].ID != "mailru:INBOX:42:2" || attachments[0].Size != 24 {
				t.Fatalf("lost body or attachment metadata: parts=%v attachments=%v", parts, attachments)
			}
			params := buffers[0].BodyStructure.(*imaplib.BodyStructureMultiPart).Extended.Params
			if tc.name == "mailru_nil_value" {
				if _, exists := params["boundary"]; exists || params["name"] != "kept" {
					t.Fatal("missing parameter not omitted or subsequent value lost")
				}
			}
			if tc.name == "empty_string_value" {
				if value, exists := params["boundary"]; !exists || value != "" {
					t.Fatal("empty string confused with missing value")
				}
			}
		})
	}
}
