# typemail

`typemail` classifies the messages in an IMAP `INBOX` with TypeSafe Jev through OpenRouter. It is a read-only scanner with a small Go library and CLI.

## Quick start

Set the mailbox and OpenRouter credentials:

```sh
export TYPEMAIL_IMAP_ADDR=imap.example.com:993
export TYPEMAIL_IMAP_USERNAME=alice
export TYPEMAIL_IMAP_PASSWORD='password'
export OPENROUTER_API_KEY=your-key
```

Run the scanner:

```sh
go run ./cmd/typemail
go run ./cmd/typemail -concurrency 10
```

The default concurrency is five. Values below one are rejected. A message fetch or classification failure is printed to stderr, scanning continues, and the command exits nonzero after the summary.

## Classification categories

- `legitimate`: expected personal, business, or transactional correspondence
- `marketing`: promotional mail or newsletters from an identifiable sender
- `spam`: unwanted bulk mail, generic solicitation, or low-quality junk
- `phishing`: deceptive requests for credentials, money, sensitive data, or harmful actions
- `suspicious`: concerning or ambiguous mail that does not clearly fit another category

Each successful result includes the selected class, confidence, all five probabilities, and the concrete model identifier returned by OpenRouter.

## Read-only processing

The CLI selects `INBOX` with IMAP `EXAMINE` and fetches messages with `BODY.PEEK[]`. It does not set flags, rewrite messages, move mail, or delete mail. IMAP fetching and MIME parsing are sequential. Only OpenRouter calls run concurrently.

The parser decodes MIME headers, transfer encodings, and supported character sets. It keeps inline `text/plain` and `text/html` parts, ignores attachments, and caps the combined decoded text at 100 KiB. It drains the source after reaching the cap so the IMAP connection can be reused.

The scanner sends a `POST` request to `https://openrouter.ai/api/v1/systemone` using the `~typesafe/jev-latest` model. The following headers are sent with the decoded inline body text to OpenRouter and TypeSafe: `From`, `Reply-To`, `To`, `Subject`, `Date`, `List-ID`, `List-Unsubscribe`, and `Authentication-Results`. Email content is treated as data for classification, not as instructions to the classifier.

## Library

```go
message, err := typemail.ParseMessage(reader, 100<<10)
classifier, err := typemail.NewClassifier(apiKey)
result, err := classifier.Classify(ctx, message)
```

The library uses `github.com/emersion/go-imap/v2` for IMAP integration and `github.com/emersion/go-message` for RFC 822 and MIME parsing. It does not include an OpenRouter SDK.

## Current exclusions

- Postfix or Cyrus integration
- Subject or header rewriting
- Moving or deleting messages
- Attachment inspection
- OAuth authentication
- Persistent checkpoints or incremental scans
- Generic TypeSafe or OpenRouter SDK functionality
- Policies that act on messages

## Validation

```sh
gofmt -w .
go test ./...
go vet ./...
```

A real-mailbox check is still required to verify the IMAP server's read-only behavior and credentials.
