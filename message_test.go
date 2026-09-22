package typemail

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

type trackingReader struct {
	reader *bytes.Reader
	done   bool
}

func (r *trackingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err == io.EOF || r.reader.Len() == 0 {
		r.done = true
	}
	return n, err
}

func TestParseMessagePlainText(t *testing.T) {
	message, err := ParseMessage(strings.NewReader("From: sender@example.com\r\nSubject: Hello\r\n\r\nBody text"), 100)
	if err != nil {
		t.Fatalf("ParseMessage() error = %v", err)
	}
	if got := message.Headers["From"]; len(got) != 1 || got[0] != "sender@example.com" {
		t.Fatalf("From = %#v", got)
	}
	if got := message.Headers["Subject"]; len(got) != 1 || got[0] != "Hello" {
		t.Fatalf("Subject = %#v", got)
	}
	if len(message.TextParts) != 1 || message.TextParts[0].ContentType != "text/plain" || message.TextParts[0].Text != "Body text" {
		t.Fatalf("TextParts = %#v", message.TextParts)
	}
}

func TestParseMessageMultipartTextAndHTML(t *testing.T) {
	raw := "From: sender@example.com\r\n" +
		"To: recipient@example.com\r\n" +
		"Content-Type: multipart/alternative; boundary=part\r\n\r\n" +
		"--part\r\nContent-Type: text/plain\r\n\r\nPlain\r\n" +
		"--part\r\nContent-Type: text/html\r\n\r\n<b>HTML</b>\r\n" +
		"--part--\r\n"

	message, err := ParseMessage(strings.NewReader(raw), 100)
	if err != nil {
		t.Fatalf("ParseMessage() error = %v", err)
	}
	if len(message.TextParts) != 2 {
		t.Fatalf("TextParts length = %d, want 2", len(message.TextParts))
	}
	if message.TextParts[0].Text != "Plain" || message.TextParts[1].Text != "<b>HTML</b>" {
		t.Fatalf("TextParts = %#v", message.TextParts)
	}
}

func TestParseMessageDecodesEncodedSubjectAndBody(t *testing.T) {
	raw := "Subject: =?UTF-8?B?SGVsbG8g4piD?=\r\n" +
		"Content-Type: text/plain; charset=iso-8859-1\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"caf=E9"

	message, err := ParseMessage(strings.NewReader(raw), 100)
	if err != nil {
		t.Fatalf("ParseMessage() error = %v", err)
	}
	if got := message.Headers["Subject"][0]; got != "Hello ☃" {
		t.Fatalf("Subject = %q", got)
	}
	if got := message.TextParts[0].Text; got != "café" {
		t.Fatalf("body = %q", got)
	}
}

func TestParseMessageExcludesAttachments(t *testing.T) {
	raw := "Content-Type: multipart/mixed; boundary=part\r\n\r\n" +
		"--part\r\nContent-Type: text/plain\r\n\r\nKeep\r\n" +
		"--part\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=secret.txt\r\n\r\nDo not send\r\n" +
		"--part--\r\n"

	message, err := ParseMessage(strings.NewReader(raw), 100)
	if err != nil {
		t.Fatalf("ParseMessage() error = %v", err)
	}
	if len(message.TextParts) != 1 || message.TextParts[0].Text != "Keep" {
		t.Fatalf("TextParts = %#v", message.TextParts)
	}
}

func TestParseMessageCapsCombinedTextAndDrainsSource(t *testing.T) {
	raw := "Content-Type: multipart/alternative; boundary=part\r\n\r\n" +
		"--part\r\nContent-Type: text/plain\r\n\r\n123456\r\n" +
		"--part\r\nContent-Type: text/html\r\n\r\nabcdef\r\n" +
		"--part--\r\n"
	reader := &trackingReader{reader: bytes.NewReader([]byte(raw))}

	message, err := ParseMessage(reader, 8)
	if err != nil {
		t.Fatalf("ParseMessage() error = %v", err)
	}
	var total int
	for _, part := range message.TextParts {
		total += len(part.Text)
	}
	if total != 8 {
		t.Fatalf("decoded text bytes = %d, want 8", total)
	}
	if !reader.done {
		t.Fatal("source reader was not consumed")
	}
}

func TestParseMessageRejectsMalformedMessage(t *testing.T) {
	if _, err := ParseMessage(strings.NewReader("not a header\r\n\r\nbody"), 100); err == nil {
		t.Fatal("ParseMessage() error = nil, want error")
	}
}

func TestParseMessageRejectsNegativeLimit(t *testing.T) {
	if _, err := ParseMessage(strings.NewReader("\r\nbody"), -1); err == nil {
		t.Fatal("ParseMessage() error = nil, want error")
	}
}
