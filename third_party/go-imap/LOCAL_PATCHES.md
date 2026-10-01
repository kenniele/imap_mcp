# Local compatibility patch

Source: `github.com/emersion/go-imap/v2 v2.0.0-beta.8`, copied from the Go
module cache with the upstream license preserved. The main module retains
the pinned version and uses a local `replace` so development, CI and Docker
build the same parser.

`imapclient/fetch.go`, `readBodyFldParam`: accept `NIL` for a parameter value
and omit that missing parameter. Mail.ru returns such values in multipart
BODYSTRUCTURE extensions; upstream expects a string and closes the connection
while decoding the response. Parameter names still require strings, and
incomplete pairs remain errors. All other upstream code is unchanged.

The regression tests in `internal/imap/fetch_test.go` exercise the actual
client decoder using synthetic responses, including attachment metadata and
rejection of malformed parameter names/pairs.

Remove the local replacement when a pinned upstream release supports this
case and the regression tests pass unchanged.
