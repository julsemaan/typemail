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

- `regular`: expected personal, business, or transactional correspondence
- `selling`: email that solicits or promotes a purchase of a consumer product
- `newsletter`: expected recurring editorial or informational mail, such as news digests, articles, or community updates. Excludes purchase-focused promotions, unsolicited junk, and deceptive mail.
- `spam`: unwanted bulk mail, generic solicitation, or low-quality junk
- `phishing`: deceptive requests for credentials, money, sensitive data, or harmful actions
- `suspicious`: concerning or ambiguous mail that does not clearly fit another category

Each successful result includes the selected class, confidence, one probability per active class, and the concrete model identifier returned by OpenRouter.

## Configuration

Load an optional TOML file explicitly. The CLI does not discover files automatically, and credentials and other flags are unchanged.

```sh
go run ./cmd/typemail -config typemail.toml
```

```toml
# Optional sender bypass. Keep top-level settings before class sections.
skip_sender_domains = ["semaan.ca"]

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

Names are exact and case-sensitive. Blank names, surrounding whitespace, and control characters are rejected. Explicitly blank criteria, unknown settings, malformed TOML, and configurations with no active classes are also rejected before connecting to IMAP. No file, an empty file, or an empty `classes` table preserves class defaults.

### Sender-domain bypass

The optional top-level `skip_sender_domains` list skips classification for messages whose `From` header has a matching sender domain, case-insensitively and including subdomains. For example, `semaan.ca` also matches `mail.semaan.ca`, but not `notsemaan.ca` or `semaan.ca.evil.com`. Missing, malformed, duplicate, or multiple-mailbox `From` headers continue through Jev. An omitted or empty list disables this bypass. Empty entries, email addresses, URLs, and wildcard patterns are invalid. Keep the setting before any `[classes.*]` sections in TOML.

Both the IMAP scanner and Milter leave skipped messages unchanged, including existing subject tags and class markers, without an OpenRouter request. The scanner reports skipped UIDs and counts them separately from classes and failures. `From` is spoofable. This is a convenience bypass, not sender authentication or a security allowlist; it does not use the SMTP envelope sender or `Reply-To`.

Requests and results use only active classes. Totals list every active class, including zero counts, and omit disabled classes.

## Subject tagging

To tag every successfully classified email, including custom classes:

```sh
go run ./cmd/typemail -config typemail.toml -rewrite-subject
```

`Your awesome email` becomes `[regular] Your awesome email`. The scanner records the class in `X-Typemail-Class`. On reruns it removes only the prefix recorded by that marker, preserves unrelated bracketed text, and skips replacement when the tag already matches. Classification excludes the recorded prefix and never sends the marker to OpenRouter.

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

To configure the library, set `SkipSenderDomains` for the sender bypass and use optional pointers for class settings to distinguish omitted values from explicit overrides:

```go
criteria := "Purchase confirmations, invoices, and payment receipts."
enabled := false
classifier, err := typemail.NewClassifierWithConfig(apiKey, typemail.ClassifierConfig{
    SkipSenderDomains: []string{"semaan.ca"},
    Classes: map[string]typemail.ClassConfig{
        "receipts":   {Criteria: &criteria},
        "suspicious": {Enabled: &enabled},
    },
})
```

`NewClassifier` preserves defaults. `NewClassifierWithConfig` validates and copies the configuration. `classifier.Classes()` returns a fresh, sorted slice of active names. Custom results use `typemail.Class` with the configured name. For a sender-domain bypass, `Classify` returns `Result.Skipped = true` with all remaining result fields empty or zero and a nil error. Callers must check `Skipped` before treating the result as a classification.

Library callers choose their own body limit through the `ParseMessage` argument; the CLI default does not apply.

The library uses `github.com/emersion/go-imap/v2` for IMAP integration and `github.com/emersion/go-message` for RFC 822 and MIME parsing. It does not include an OpenRouter SDK.

## Incoming Postfix subject tagging

`typemail-milter` is an optional, fail-open Milter for CentOS 7 and Postfix 2.10.1. It listens only on `127.0.0.1:8892`, uses protocol 2, and adds `[class]` to incoming subjects and `X-Typemail-Class` to headers. It requests only header addition and change capabilities, never body or envelope changes. The IMAP scanner is unchanged.

Only attach it to the port-25 `smtp inet` service. The daemon bypasses clients in Postfix's `mynetworks`, authenticated senders identified by `{auth_type}` or `{auth_authen}`, and unknown or non-TCP connection identities. The service override keeps submission, SMTPS, trusted outgoing relay paths, and local mail outside typemail. The Connect callback's port is the client's source port, not the receiving service port, so service placement is required.

Headers and body share a 1 MiB buffer limit. Oversized messages and duplicate Subject or class-marker headers pass unchanged. Five classifications can run at once; a sixth passes unchanged without waiting. Parsing retains the existing 16 KiB decoded inline-text limit and excludes attachments. Classification and all provider retries share a four-second deadline. Parsing, provider, rendering, cancellation, and header-write failures never request rejection, temporary failure, discard, or quarantine. Failures before modification leave headers untouched; a failed later header write can leave a partial tag. Logs contain fixed failure reasons, not message content or credentials. Selected headers and inline text still go to OpenRouter and TypeSafe as described above.

### Install on CentOS 7

Build locally on a supported Linux host with Go 1.23 or newer, Bash, and `rpmbuild`, not on CentOS 7. Install your distribution's RPM build tools, for example `rpm-build` on Fedora or `rpm` on Debian. Inspection also needs `rpm`, `rpm2cpio`, `cpio`, and `readelf` from binutils.

```sh
./deploy/build-rpm.sh 0.1.0
./deploy/check-rpm.sh dist/typemail-milter-0.1.0-1.el7.x86_64.rpm 0.1.0 1
scp dist/typemail-milter-0.1.0-1.el7.x86_64.rpm root@mail.example.com:/tmp/
```

An optional numeric release defaults to `1`, for example `./deploy/build-rpm.sh 0.1.0 2`. The build targets static Linux amd64 with `GOAMD64=v1` and creates an RPM v4 package with gzip compression. Only the binary, service unit, and public configuration template enter the package. Local `typemail.toml` and credentials are never copied.

On the CentOS 7 server, as root:

```sh
yum localinstall /tmp/typemail-milter-0.1.0-1.el7.x86_64.rpm
# Edit /etc/typemail/milter.env to contain OPENROUTER_API_KEY=your-key.
# Use an editor, not shell history. The file starts empty and is root-only.
# Optionally edit /etc/typemail/typemail.toml to customize classes and sender bypasses.
systemctl enable typemail-milter
systemctl start typemail-milter
journalctl -u typemail-milter
```

The RPM installs `/usr/bin/typemail-milter` and `/usr/lib/systemd/system/typemail-milter.service`, creates the system account and group if absent, and depends on Postfix, systemd, shadow-utils, and CA certificates. `/etc/typemail` is `root:typemail` with mode `0750`; `typemail.toml` is `root:typemail` with mode `0640`; `milter.env` is `root:root` with mode `0600`. Installation reloads systemd but never enables or starts the service and never changes Postfix configuration.

The unit uses directives available in CentOS 7's systemd 219 and runs as an unprivileged account. systemd reads the root-only environment file before dropping privileges. The configuration file uses the same strict TOML settings as the scanner, including `skip_sender_domains`. No IMAP credentials or debug flag are needed.

Startup reads `/usr/sbin/postconf -xh mynetworks` from the local Postfix configuration. Literal IPv4/IPv6 CIDRs and individual addresses are supported, including bracketed IPv6. Lookup tables or other nonliteral entries, an empty network list, invalid classifier settings, and a missing API key stop startup visibly. There is no second trusted-network list. Restart typemail whenever `mynetworks`, its configuration, or credentials change. Postfix must use the same local configuration read by `postconf`.

### Readiness and watchdog

The unit uses `Type=notify`. `systemctl start` completes only after configuration validation, Postfix network discovery, listener creation, and a successful local protocol-2 negotiation. Startup failure or cancellation never reports readiness. `TimeoutStartSec=30s` bounds startup. Inspect fixed, credential-free status messages with:

```sh
systemctl status typemail-milter
systemctl show typemail-milter -p ActiveState -p SubState -p StatusText -p MainPID -p Result
journalctl -u typemail-milter
```

`WatchdogSec=30s` checks responsiveness, not provider availability. Every ten seconds the daemon negotiates against its own listener with a one-second total deadline. It sends a heartbeat only after a valid response. Failed checks publish unhealthy status and withhold heartbeats, but continue probing so temporary failures can recover. Probes send no mail, consume no classification slots, and never call OpenRouter. Provider outages and saturation still pass mail unchanged and do not fail the watchdog.

Crashes and watchdog expiry trigger `Restart=on-failure` after five seconds. Manual stops do not restart the service. Graceful shutdown stops probes, reports `STOPPING=1`, closes the listener, and waits for the serving loop. Without `NOTIFY_SOCKET`, systemd integration is disabled and standalone behavior remains unchanged. With a notification socket, invalid watchdog durations or PIDs fail startup; `WATCHDOG_PID` must match the daemon for heartbeats to be enabled. Notification writes are bounded; readiness or heartbeat delivery failure exits nonzero, while informational status delivery is best-effort. No new CLI flags or configuration settings are needed.

### RPM upgrades, downgrade, and removal

The installed configuration is now `/etc/typemail/typemail.toml`, replacing `/etc/typemail/classes.toml`. RPM does not migrate edits across renamed paths. Before upgrading from a package using the old name, copy the existing file as root, preserving its `root:typemail` ownership and `0640` mode:

```sh
cp -p /etc/typemail/classes.toml /etc/typemail/typemail.toml
```

If the destination already exists, back it up and merge your settings rather than overwriting it. Transfer the new RPM, then upgrade as root:

```sh
yum localinstall /tmp/typemail-milter-0.1.1-1.el7.x86_64.rpm
systemctl restart typemail-milter
```

RPM upgrades preserve enablement and running state. They do not restart the daemon; explicitly restart a running service to activate the new binary. Both configuration files use `%config(noreplace)`, so local edits survive upgrades and downgrades. Review any `.rpmnew` files before merging template changes.

Before downgrading to a package using `classes.toml`, reverse the copy as root. Back up any existing destination first, then remove `skip_sender_domains` from the copied file because older versions do not support it:

```sh
cp -p /etc/typemail/typemail.toml /etc/typemail/classes.toml
# Edit /etc/typemail/classes.toml to remove skip_sender_domains, if present.
```

Retain or rebuild the older RPM, transfer it, and run:

```sh
yum downgrade /tmp/typemail-milter-0.1.0-1.el7.x86_64.rpm
systemctl restart typemail-milter
```

Before removal, follow the Postfix rollback guidance below so the port-25 service no longer references typemail. Then run `yum remove typemail-milter`. Final removal stops and disables the service and reloads systemd. The account remains; RPM saves modified configuration as `.rpmsave` files. These may contain credentials, so keep them root-only or remove them securely when no longer needed.

### Postfix configuration

These instructions target CentOS 7 and Postfix 2.10.1 and assume Postfix already receives external mail. Configuring a fresh mail server may require `main.cf` changes; integrating typemail into an already working server does not.

Back up `/etc/postfix/main.cf` and `/etc/postfix/master.cf`, including all existing port-25 overrides. Verify that Postfix sends `{auth_type}` and `{auth_authen}` at MAIL FROM under protocol 2, using `postconf milter_mail_macros` and an authenticated staging SMTP test. Preserve any existing macro settings; do not deploy without a working authentication bypass.

Under the existing port-25 `smtp inet` service in `/etc/postfix/master.cf`, add or replace only these indented overrides. Preserve the service line and unrelated overrides, and replace duplicate options rather than adding another copy:

```text
  -o smtpd_milters=inet:127.0.0.1:8892
  -o milter_protocol=2
  -o milter_default_action=accept
  -o milter_connect_timeout=1s
  -o milter_command_timeout=5s
  -o milter_content_timeout=5s
```

The example assumes no existing Milters on that service. The `smtpd_milters` override replaces the inherited list, rather than appending to it. If the service already uses Milters through `main.cf` or a service override, retain their endpoints in a comma-separated list with `inet:127.0.0.1:8892` and review their order and signing or verification roles. Postfix applies Milters in the listed order. Do not blindly replace an existing chain with the example.

Leave global `smtpd_milters`, `non_smtpd_milters`, submission, SMTPS, and all other services unchanged. Preserve all restrictions, security filters, and any existing content-filter configuration; SpamAssassin is not required. Check the configuration with `postfix check`, then `postfix reload`.

The protocol, failure-action, and timeout overrides affect every Milter attached to the port-25 service, not just typemail. Confirm that each existing Milter supports protocol 2 and the proposed fail-open policy and timeouts before enabling this. The timeouts bound individual Milter stages, not the entire SMTP transaction. `accept` is a fallback for unavailable or failed filters, not an override of existing security-policy rejections. Subject changes can invalidate DKIM signatures during later verification, or signatures created by an earlier Milter. Understand the complete signing and authentication pipeline before enabling this.

Protocol and macro references: Postfix 2.10.1's [master.cf syntax](https://github.com/vdukhovni/postfix/blob/v2.10.1/postfix/man/man5/master.5), [Milter support](https://github.com/vdukhovni/postfix/blob/v2.10.1/postfix/README_FILES/MILTER_README), [parameter definitions](https://github.com/vdukhovni/postfix/blob/v2.10.1/postfix/proto/postconf.proto), and the pinned [go-milter v0.4.1 implementation](https://github.com/emersion/go-milter/tree/v0.4.1). The library implements protocol 2; continuation responses preserve connection identity between transactions.

### Staging and rollback

On disposable CentOS 7 staging with Postfix 2.10.1, run `postfix check` and test both a setup with no existing Milters and an existing-filter chain before production:

- Untrusted unauthenticated port-25 mail receives one tag, including custom classes, encoded subjects, and messages without a subject.
- Trusted, authenticated, submission, SMTPS, and local messages remain untouched by typemail.
- Test missing credentials, invalid configuration or `mynetworks`, and an occupied listener port. Confirm failed starts never reach `active/running` or publish readiness. Restore each fault before the next test.
- Record `MainPID`, send SIGKILL, and confirm a new PID starts after the five-second restart delay. Repeat with SIGSTOP and wait past the 30-second watchdog window plus restart delay. Confirm watchdog failure in the journal and a new ready PID. Do not resume the obsolete PID after restart.
- Run `systemctl stop typemail-milter` and wait beyond the restart delay. Confirm it remains stopped, then start it again. During stop, crash, and freeze tests, confirm delivery without classifier-related rejection or retry.
- Simulate provider outages and hangs. Confirm unchanged delivery when slots fill or the four-second deadline expires.
- Compare delivered MIME bodies and attachments with the originals. If your installation reinjects mail, verify that reinjection never adds another typemail tag and does not route mail through typemail again.
- Check DKIM verification, authentication results, and restrictions. If existing filters or signing are configured, confirm their expected behavior under the shared protocol, failure-action, and timeout overrides.

Rollback by restoring the previous port-25 overrides from the backup, then running `postfix check` and `postfix reload`. Leave global settings and all other services unchanged. After reload, stop and disable `typemail-milter`. Already delivered tags are not removed by rollback.

Runtime validation on disposable CentOS 7 staging remains unverified. Local tests cannot establish real systemd readiness, watchdog restart, manual-stop behavior, delivery, bypass, fail-open, or existing-filter compatibility.

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
./deploy/build-rpm.sh 0.1.0
./deploy/check-rpm.sh dist/typemail-milter-0.1.0-1.el7.x86_64.rpm 0.1.0 1
```

RPM inspection checks metadata, dependencies, the exact file list, ownership, modes, configuration flags, gzip payload, and static binary linkage. It compares the packaged templates with the public sources and checks that the environment file is empty.

CentOS 7 package compatibility is accepted only after testing on a disposable CentOS 7 system with real systemd and Postfix. Test fresh RPM installation, upgrade, downgrade, and final removal. Edit both configuration files first and confirm preservation, `.rpmnew` or `.rpmsave` handling, root-only credential permissions, and retention of the service account. Check that installation leaves the service disabled and stopped, upgrades and downgrades preserve its prior running and enablement state, and removal stops and disables it. Confirm startup uses `/usr/bin/typemail-milter`, compare Postfix configuration against a pre-install backup, and run the fail-open staging checks above. Local inspection alone does not establish CentOS 7 compatibility.

Protocol-2 wire tests cover header tagging, bypasses, repeated transactions and aborts, MIME limits, failures, retries, cancellation, and saturation. They cannot verify the real Postfix pipeline or any existing filters; the staging checks above remain required.

Local scripted IMAP tests cover read-only defaults, capabilities, targeted replacement and expunge, failure paths, cancellation, FETCH draining, and serialization. A disposable real-mailbox check is still required to verify server behavior, flag and date preservation, client refreshes, and unchanged attachments.
