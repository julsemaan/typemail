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
go run ./cmd/typemail -max-email-bytes 8192
```

The default concurrency is five. Values below one are rejected. A message fetch or classification failure is printed to stderr, scanning continues, and the command exits nonzero after the summary.

## HTTP diagnostics

Provider explanations are hidden by default. To print HTTP failure details alongside each failed UID, including the requested model and state and serialized request byte counts, run:

```sh
go run ./cmd/typemail -debug
```

Diagnostics decode only the provider's `error.message` from responses up to 4 KiB and redact the configured API key. Unavailable details have a reason instead of raw response content. Provider explanations may quote submitted email content. Do not share diagnostic output without reviewing it.

## Classification categories

- `legitimate`: expected personal, business, or transactional correspondence
- `selling`: email that solicits or promotes a purchase of a consumer product
- `newsletter`: expected recurring editorial or informational mail, such as news digests, articles, or community updates. Excludes purchase-focused promotions, unsolicited junk, and deceptive mail.
- `spam`: unwanted bulk mail, generic solicitation, or low-quality junk
- `phishing`: deceptive requests for credentials, money, sensitive data, or harmful actions
- `suspicious`: concerning or ambiguous mail that does not clearly fit another category

Each successful result includes the selected class, confidence, one probability per active class, and the concrete model identifier returned by OpenRouter.

## Class configuration

Load an optional TOML file explicitly. The CLI does not discover files automatically, and credentials and other flags are unchanged.

```sh
go run ./cmd/typemail -config typemail.toml
```

```toml
# Replace a built-in's criteria.
[classes.selling]
criteria = """
Emails promoting purchases.
Exclude purchase confirmations and receipts.
"""

# Disable a built-in.
[classes.suspicious]
enabled = false

# Add a custom class.
[classes.receipts]
criteria = "Purchase confirmations, invoices, and payment receipts."
```

All built-ins start enabled with their existing criteria. Unmentioned built-ins and empty built-in sections remain unchanged. `enabled` defaults to `true` for every class. Supplied criteria replace the entire string. Custom classes require nonblank criteria even when disabled.

Names are exact and case-sensitive. Blank names, surrounding whitespace, and control characters are rejected. Explicitly blank criteria, unknown settings, malformed TOML, and configurations with no active classes are also rejected before connecting to IMAP. No file, an empty file, or an empty `classes` table preserves defaults.

Requests and results use only active classes. Totals list every active class, including zero counts, and omit disabled classes.

## Read-only processing

The CLI selects `INBOX` with IMAP `EXAMINE` and fetches messages with `BODY.PEEK[]`. It does not set flags, rewrite messages, move mail, or delete mail. IMAP fetching and MIME parsing are sequential. Only OpenRouter calls run concurrently.

The parser decodes MIME headers, transfer encodings, and supported character sets. It keeps inline `text/plain` and `text/html` parts and ignores attachments. The CLI keeps the first 16,384 bytes of combined decoded body text in MIME order by default. Use `-max-email-bytes` to change this limit; values below one are rejected. The limit excludes headers and JSON overhead. The parser drains the source after reaching the cap so the IMAP connection can be reused.

The scanner sends a `POST` request to `https://openrouter.ai/api/v1/systemone` using the `~typesafe/jev-latest` model. The following headers are sent with the decoded inline body text to OpenRouter and TypeSafe: `From`, `Reply-To`, `To`, `Subject`, `Date`, `List-ID`, `List-Unsubscribe`, and `Authentication-Results`. Email content is treated as data for classification, not as instructions to the classifier.

## Library

```go
message, err := typemail.ParseMessage(reader, 100<<10)
classifier, err := typemail.NewClassifier(apiKey)
result, err := classifier.Classify(ctx, message)
```

To configure library classes, use optional pointers to distinguish omitted values from explicit overrides:

```go
criteria := "Purchase confirmations, invoices, and payment receipts."
enabled := false
classifier, err := typemail.NewClassifierWithConfig(apiKey, typemail.ClassifierConfig{
    Classes: map[string]typemail.ClassConfig{
        "receipts":   {Criteria: &criteria},
        "suspicious": {Enabled: &enabled},
    },
})
```

`NewClassifier` preserves defaults. `NewClassifierWithConfig` validates and copies the configuration. `classifier.Classes()` returns a fresh, sorted slice of active names. Custom results use `typemail.Class` with the configured name.

Library callers choose their own body limit through the `ParseMessage` argument; the CLI default does not apply.

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
go test -race ./...
go vet ./...
```

A real-mailbox check is still required to verify the IMAP server's read-only behavior and credentials.
# typemail
