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

## Incoming Postfix subject tagging

`typemail-milter` is an optional, fail-open Milter for CentOS 7 and Postfix 2.10.1. It listens only on `127.0.0.1:8892`, uses protocol 2, and adds `[class]` to incoming subjects and `X-Typemail-Class` to headers. It requests only header addition and change capabilities, never body or envelope changes. The IMAP scanner is unchanged.

Only attach it to the port-25 `smtp inet` service. The daemon bypasses clients in Postfix's `mynetworks`, authenticated senders identified by `{auth_type}` or `{auth_authen}`, and unknown or non-TCP connection identities. The service override keeps submission, SMTPS, trusted outgoing relay paths, and local mail outside typemail. The Connect callback's port is the client's source port, not the receiving service port, so service placement is required.

Headers and body share a 1 MiB buffer limit. Oversized messages and duplicate Subject or class-marker headers pass unchanged. Five classifications can run at once; a sixth passes unchanged without waiting. Parsing retains the existing 16 KiB decoded inline-text limit and excludes attachments. Classification and all provider retries share a four-second deadline. Parsing, provider, rendering, cancellation, and header-write failures never request rejection, temporary failure, discard, or quarantine. Failures before modification leave headers untouched; a failed later header write can leave a partial tag. Logs contain fixed failure reasons, not message content or credentials. Selected headers and inline text still go to OpenRouter and TypeSafe as described above.

### Install on CentOS 7

Build a static Linux amd64 binary on a supported build host with Go 1.23 or newer, not on CentOS 7:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o typemail-milter ./cmd/typemail-milter
```

Copy that binary and `deploy/typemail-milter.service` to the server. As root:

```sh
useradd --system --no-create-home --shell /sbin/nologin typemail
install -m 0755 typemail-milter /usr/local/bin/typemail-milter
install -d -o root -g typemail -m 0750 /etc/typemail
install -o root -g typemail -m 0640 typemail.toml /etc/typemail/classes.toml
install -o root -g root -m 0600 /dev/null /etc/typemail/milter.env
# Edit milter.env to contain OPENROUTER_API_KEY=your-key. Do not put it in shell history.
install -m 0644 deploy/typemail-milter.service /etc/systemd/system/typemail-milter.service
systemctl daemon-reload
systemctl enable typemail-milter
systemctl start typemail-milter
journalctl -u typemail-milter
```

The unit uses directives available in CentOS 7's systemd 219 and runs as an unprivileged account. systemd reads the root-only environment file before dropping privileges. The class file uses the same strict TOML settings as the scanner. No IMAP credentials or debug flag are needed.

Startup reads `/usr/sbin/postconf -xh mynetworks` from the local Postfix configuration. Literal IPv4/IPv6 CIDRs and individual addresses are supported, including bracketed IPv6. Lookup tables or other nonliteral entries, an empty network list, invalid classifier settings, and a missing API key stop startup visibly. There is no second trusted-network list. Restart typemail whenever `mynetworks`, its class configuration, or credentials change. Postfix must use the same local configuration read by `postconf`.

### Postfix configuration

Back up `/etc/postfix/main.cf` and `/etc/postfix/master.cf`, including any existing port-25 timeout overrides. Identify the existing filter on port 8891 and its signing or verification role before deploying. Verify that Postfix sends `{auth_type}` and `{auth_authen}` at MAIL FROM under protocol 2, using `postconf milter_mail_macros` and an authenticated staging SMTP test. Preserve any existing macro settings; do not deploy without a working authentication bypass.

Keep these existing `main.cf` values unchanged:

```text
smtpd_milters = inet:127.0.0.1:8891
non_smtpd_milters = $smtpd_milters
milter_default_action = accept
milter_protocol = 2
```

Retain the existing `smtp inet` line and its `-o content_filter=spamassassin` in `master.cf`. Add or replace only these overrides on that service, without duplicate options:

```text
  -o smtpd_milters=inet:127.0.0.1:8891,inet:127.0.0.1:8892
  -o milter_default_action=accept
  -o milter_connect_timeout=1s
  -o milter_command_timeout=5s
  -o milter_content_timeout=5s
```

Do not change submission, SMTPS, `smtpsemaan`, or non-SMTP filtering. SpamAssassin's `sendmail` reinjection uses the unchanged `non_smtpd_milters`, so it does not invoke typemail again. Preserve all restrictions and security filters. Check the configuration with `postfix check`, then `postfix reload`.

The five-second command/content timeouts also apply to the existing inbound Milter at 8891. These bound individual Milter stages, not the entire SMTP transaction. `accept` is a fallback for unavailable or failed filters, not an override of existing security-policy rejections. Subject changes can invalidate DKIM signatures during later verification, or signatures created by the earlier Milter. Understand the complete signing and authentication pipeline before enabling this.

Protocol and macro references: [Postfix Milter support](https://www.postfix.org/MILTER_README.html), [Postfix parameters](https://www.postfix.org/postconf.5.html#milter_protocol), and the pinned [go-milter v0.4.1 implementation](https://github.com/emersion/go-milter/tree/v0.4.1). The library implements protocol 2; continuation responses preserve connection identity between transactions.

### Staging and rollback

On staging Postfix 2.10.1, check the complete SpamAssassin pipeline before production:

- Untrusted unauthenticated port-25 mail receives one tag, including custom classes, encoded subjects, and messages without a subject.
- Trusted, authenticated, submission, SMTPS, and local messages remain untouched by typemail.
- Stop, kill, and freeze the daemon. Confirm delivery without classifier-related rejection or retry. For a freeze test, send SIGSTOP and restore with SIGCONT afterward.
- Simulate provider outages and hangs. Confirm unchanged delivery when slots fill or the four-second deadline expires.
- Compare delivered MIME bodies and attachments with the originals. Verify that reinjection never adds another typemail tag.
- Check existing signing, DKIM verification, authentication results, restrictions, and the filter at 8891 for expected behavior under the shared timeouts.

Rollback by restoring the previous port-25 Milter overrides and timeout values from the backup, then running `postfix check` and `postfix reload`. Leave the original 8891 and non-SMTP configuration intact. After reload, stop and disable `typemail-milter`. Already delivered tags are not removed by rollback.

## Current exclusions

- Cyrus-specific integration
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
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/typemail-milter
```

Protocol-2 wire tests cover header tagging, bypasses, repeated transactions and aborts, MIME limits, failures, retries, cancellation, and saturation. They cannot verify the real Postfix/SpamAssassin pipeline; the staging checks above remain required.

Local scripted IMAP tests cover read-only defaults, capabilities, targeted replacement and expunge, failure paths, cancellation, FETCH draining, and serialization. A disposable real-mailbox check is still required to verify server behavior, flag and date preservation, client refreshes, and unchanged attachments.
