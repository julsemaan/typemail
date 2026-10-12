package typemail

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"
)

// Message is the bounded, attachment-free representation sent for classification.
type Message struct {
	Headers   map[string][]string
	TextParts []TextPart
}

// TextPart is one inline text/plain or text/html MIME part.
type TextPart struct {
	ContentType string
	Text        string
}

var selectedHeaders = map[string]string{
	"from":                   "From",
	"reply-to":               "Reply-To",
	"to":                     "To",
	"subject":                "Subject",
	"date":                   "Date",
	"list-id":                "List-ID",
	"list-unsubscribe":       "List-Unsubscribe",
	"authentication-results": "Authentication-Results",
}

const classMarkerHeader = "X-Typemail-Class"

func stripClassPrefix(subject, marker string) string {
	if marker == "" {
		return subject
	}
	if subject == "["+marker+"]" {
		return ""
	}
	return strings.TrimPrefix(subject, "["+marker+"] ")
}

// RewriteSubject prefixes the decoded subject with class and records its marker.
// The raw MIME body and unrelated headers are preserved. An unchanged message
// is consumed without writing to w.
func RewriteSubject(w io.Writer, r io.Reader, class Class) (subject string, changed bool, err error) {
	if err := validateClassName(string(class)); err != nil {
		return "", false, err
	}
	if w == nil || r == nil {
		return "", false, fmt.Errorf("rewrite subject: nil writer or reader")
	}
	reader := bufio.NewReader(r)
	rawHeader, err := textproto.ReadHeader(reader)
	if err != nil {
		return "", false, fmt.Errorf("read message header: %w", err)
	}
	header := mail.Header{Header: message.Header{Header: rawHeader}}
	subject, err = header.Subject()
	if err != nil {
		return "", false, fmt.Errorf("decode subject: %w", err)
	}
	marker, err := header.Text(classMarkerHeader)
	if err != nil {
		return "", false, fmt.Errorf("decode class marker: %w", err)
	}
	label := "[" + string(class) + "]"
	if marker == string(class) && (subject == label || strings.HasPrefix(subject, label+" ")) {
		_, err := io.Copy(io.Discard, reader)
		return subject, false, err
	}
	subject = stripClassPrefix(subject, marker)
	if subject == "" {
		subject = label
	} else {
		subject = label + " " + subject
	}
	header.SetSubject(subject)
	header.SetText(classMarkerHeader, string(class))
	if err := textproto.WriteHeader(w, header.Header.Header); err != nil {
		return subject, false, fmt.Errorf("write message header: %w", err)
	}
	if _, err := io.Copy(w, reader); err != nil {
		return subject, false, fmt.Errorf("copy message body: %w", err)
	}
	return subject, true, nil
}

// ParseMessage parses an RFC 822 message, excluding attachments and limiting
// the combined decoded inline text to maxTextBytes.
func ParseMessage(r io.Reader, maxTextBytes int64) (Message, error) {
	if r == nil {
		return Message{}, fmt.Errorf("parse message: nil reader")
	}
	if maxTextBytes < 0 {
		return Message{}, fmt.Errorf("parse message: negative text limit")
	}

	reader, err := mail.CreateReader(r)
	if err != nil {
		return Message{}, fmt.Errorf("parse message: %w", err)
	}

	marker, err := reader.Header.Text(classMarkerHeader)
	if err != nil {
		return Message{}, fmt.Errorf("decode class marker: %w", err)
	}
	result := Message{Headers: make(map[string][]string)}
	fields := reader.Header.Fields()
	for fields.Next() {
		key, ok := selectedHeaders[strings.ToLower(fields.Key())]
		if !ok {
			continue
		}
		value, err := fields.Text()
		if err != nil {
			return Message{}, fmt.Errorf("decode %s header: %w", key, err)
		}
		if key == "Subject" {
			value = stripClassPrefix(value, marker)
		}
		result.Headers[key] = append(result.Headers[key], value)
	}

	var used int64
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Message{}, fmt.Errorf("read message part: %w", err)
		}

		inline, ok := part.Header.(*mail.InlineHeader)
		if !ok {
			if _, err := io.Copy(io.Discard, part.Body); err != nil {
				return Message{}, fmt.Errorf("discard attachment: %w", err)
			}
			continue
		}

		contentType, _, err := inline.ContentType()
		if err != nil {
			return Message{}, fmt.Errorf("read content type: %w", err)
		}
		contentType = strings.ToLower(strings.TrimSpace(contentType))
		if contentType != "text/plain" && contentType != "text/html" {
			if _, err := io.Copy(io.Discard, part.Body); err != nil {
				return Message{}, fmt.Errorf("discard inline part: %w", err)
			}
			continue
		}

		var body bytes.Buffer
		remaining := maxTextBytes - used
		if remaining > 0 {
			written, err := io.Copy(&body, io.LimitReader(part.Body, remaining))
			used += written
			if err != nil {
				return Message{}, fmt.Errorf("read %s part: %w", contentType, err)
			}
		}
		if _, err := io.Copy(io.Discard, part.Body); err != nil {
			return Message{}, fmt.Errorf("drain %s part: %w", contentType, err)
		}

		result.TextParts = append(result.TextParts, TextPart{
			ContentType: contentType,
			Text:        body.String(),
		})
	}

	if err := reader.Close(); err != nil {
		return Message{}, fmt.Errorf("close message reader: %w", err)
	}
	return result, nil
}
