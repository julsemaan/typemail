# typemail

`typemail` classifies the messages in an IMAP `INBOX` with TypeSafe Jev through OpenRouter. It is read-only by default, with opt-in subject tagging and a small Go library and CLI.

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

## Subject tagging

To tag every successfully classified email, including custom classes:

```sh
go run ./cmd/typemail -config typemail.toml -rewrite-subject
```

`Your awesome email` becomes `[legitimate] Your awesome email`. The scanner records the class in `X-Typemail-Class`. On reruns it removes only the prefix recorded by that marker, preserves unrelated bracketed text, and skips replacement when the tag already matches. Classification excludes the recorded prefix and never sends the marker to OpenRouter.

Rewriting requires UIDPLUS or IMAP4rev2 and selects `INBOX` read-write. The scanner snapshots non-deleted UIDs once, so uploaded replacements do not enter the same scan. Classification remains concurrent; one mutex serializes complete IMAP fetch and replacement operations.

IMAP messages are immutable. The scanner streams each rewritten email through a private temporary file, uploads a replacement preserving the raw MIME body, persistent flags and internal date, then deletes and UID-expunges only the original. It never uses mailbox-wide EXPUNGE. Successful output shows the replacement UID and tagged subject. Missing or newly deleted messages skip rewriting. Rewrite failures count as failures and cause a nonzero exit.

Before using this on important mail, test with a disposable mailbox:

- Replacement messages have new UIDs. Clients may display duplicates or stale cached subjects until they refresh.
- Changing headers can invalidate DKIM signatures.
- Flags reflect the last fetch and can race with another client. `\\Recent` and `\\Deleted` are not copied.
- Interruptions or ambiguous APPEND outcomes can leave duplicates. The original remains untouched until the upload completes and the server confirms a replacement UID with matching UIDVALIDITY. There is no automatic APPEND retry or persistent recovery. If original cleanup fails, the error reports both UIDs; inspect them before retrying.
- Do not run multiple rewrite scanners simultaneously.

## Read-only processing

Without `-rewrite-subject`, the CLI selects `INBOX` with IMAP `EXAMINE` and fetches messages with `BODY.PEEK[]`. It does not set flags, rewrite messages, move mail, or delete mail. IMAP fetching and MIME parsing are sequential. Only OpenRouter calls run concurrently. Scans exclude messages already marked deleted.

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
- Moving messages or deleting them outside subject replacement
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

Local scripted IMAP tests cover read-only defaults, capabilities, targeted replacement and expunge, failure paths, cancellation, FETCH draining, and serialization. A disposable real-mailbox check is still required to verify server behavior, flag and date preservation, client refreshes, and unchanged attachments.
