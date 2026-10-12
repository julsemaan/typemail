package typemail

import (
	"bufio"
	"bytes"
	"io"
	"mime"
	"strings"
	"testing"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"
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
	message, err := ParseMessage(strings.NewReader("From: sender@example.com\r\nList-ID: <list.example>\r\nSubject: Hello\r\n\r\nBody text"), 100)
	if err != nil {
		t.Fatalf("ParseMessage() error = %v", err)
	}
	if got := message.Headers["From"]; len(got) != 1 || got[0] != "sender@example.com" {
		t.Fatalf("From = %#v", got)
	}
	if got := message.Headers["Subject"]; len(got) != 1 || got[0] != "Hello" {
		t.Fatalf("Subject = %#v", got)
	}
	if got := message.Headers["List-ID"]; len(got) != 1 || got[0] != "<list.example>" {
		t.Fatalf("List-ID = %#v", got)
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

func TestRewriteSubject(t *testing.T) {
	for _, tc := range []struct {
		name, headers, want string
		class               Class
	}{
		{"plain", "Subject: Hello\r\n", "[spam] Hello", ClassSpam},
		{"unicode", "Subject: =?UTF-8?B?SGVsbG8g4piD?=\r\n", "[spam] Hello ☃", ClassSpam},
		{"folded", "Subject: Hello\r\n world\r\n", "[spam] Hello world", ClassSpam},
		{"missing", "", "[spam]", ClassSpam},
		{"custom", "Subject: Hello\r\n", "[équipe] Hello", Class("équipe")},
		{"changed class", "Subject: [spam] Hello\r\nX-Typemail-Class: spam\r\n", "[phishing] Hello", ClassPhishing},
		{"encoded marker class change", "Subject: " + mime.QEncoding.Encode("utf-8", "[équipe] Hello") + "\r\nX-Typemail-Class: " + mime.QEncoding.Encode("utf-8", "équipe") + "\r\n", "[spam] Hello", ClassSpam},
		{"unmarked prefix", "Subject: [spam] Hello\r\n", "[spam] [spam] Hello", ClassSpam},
		{"mismatched marker", "Subject: Hello\r\nX-Typemail-Class: spam\r\n", "[spam] Hello", ClassSpam},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := "--part\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\ncaf=C3=A9\r\n--part\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\nContent-Disposition: attachment; filename=a.bin\r\n\r\nAAH/\r\n--part--\r\nepilogue\x00\xff"
			raw := tc.headers + "X-Custom: untouched\r\n folded\r\nContent-Type: multipart/mixed; boundary=part\r\n\r\n" + body
			var out bytes.Buffer
			subject, changed, err := RewriteSubject(&out, strings.NewReader(raw), tc.class)
			if err != nil || !changed || subject != tc.want {
				t.Fatalf("RewriteSubject() = %q, %v, %v; want %q, true, nil", subject, changed, err, tc.want)
			}
			r := bufio.NewReader(bytes.NewReader(out.Bytes()))
			h, err := textproto.ReadHeader(r)
			if err != nil {
				t.Fatal(err)
			}
			header := mail.Header{Header: message.Header{Header: h}}
			if got, err := header.Subject(); err != nil || got != tc.want {
				t.Fatalf("Subject = %q, %v", got, err)
			}
			if got, err := header.Text("X-Typemail-Class"); err != nil || got != string(tc.class) {
				t.Fatalf("marker = %q, %v", got, err)
			}
			gotBody, err := io.ReadAll(r)
			if err != nil || string(gotBody) != body {
				t.Fatalf("body changed: %v", err)
			}
			if !strings.Contains(out.String(), "X-Custom: untouched\r\n folded\r\n") {
				t.Fatal("unrelated header changed")
			}
			// A rerun must drain a body larger than the header reader's buffer.
			rerun := &trackingReader{reader: bytes.NewReader(append(out.Bytes(), bytes.Repeat([]byte("x"), 10000)...))}
			var unchanged bytes.Buffer
			got, changed, err := RewriteSubject(&unchanged, rerun, tc.class)
			if err != nil || changed || got != tc.want || !rerun.done {
				t.Fatalf("rerun = %q, %v, %v, drained=%v", got, changed, err, rerun.done)
			}
			if unchanged.Len() != 0 {
				t.Fatal("unchanged message was written")
			}
		})
	}
}

func TestRewriteSubjectInvalidClasses(t *testing.T) {
	for _, class := range []Class{"", " spam", "spam ", "spam\r\nInjected: yes", "spam\x00", "spam\x7f"} {
		var out bytes.Buffer
		if _, _, err := RewriteSubject(&out, strings.NewReader("Subject: Hello\r\n\r\nBody"), class); err == nil {
			t.Fatalf("class %q accepted", class)
		}
		if out.Len() != 0 {
			t.Fatal("invalid class wrote output")
		}
	}
}

func TestParseMessageStripsOnlyMarkedPrefix(t *testing.T) {
	for _, tc := range []struct{ subject, marker, want string }{
		{"[spam] Hello", "spam", "Hello"},
		{"[équipe] Hello ☃", "équipe", "Hello ☃"},
		{"[spam] Hello", "", "[spam] Hello"},
		{"[spam] Hello", "phishing", "[spam] Hello"},
	} {
		raw := "Subject: " + mime.QEncoding.Encode("utf-8", tc.subject) + "\r\nX-Typemail-Class: " + mime.QEncoding.Encode("utf-8", tc.marker) + "\r\n\r\nBody"
		msg, err := ParseMessage(strings.NewReader(raw), 100)
		if err != nil {
			t.Fatal(err)
		}
		if got := msg.Headers["Subject"][0]; got != tc.want {
			t.Fatalf("Subject = %q, want %q", got, tc.want)
		}
		if _, ok := msg.Headers["X-Typemail-Class"]; ok {
			t.Fatal("marker included in classification")
		}
	}
}

func TestParseMessageRejectsNegativeLimit(t *testing.T) {
	if _, err := ParseMessage(strings.NewReader("\r\nbody"), -1); err == nil {
		t.Fatal("ParseMessage() error = nil, want error")
	}
}
