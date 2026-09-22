package typemail

import (
	"bytes"
	"fmt"
	"io"
	"net/textproto"
	"strings"

	_ "github.com/emersion/go-message/charset"
	"github.com/emersion/go-message/mail"
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

var selectedHeaders = map[string]struct{}{
	"From":                   {},
	"Reply-To":               {},
	"To":                     {},
	"Subject":                {},
	"Date":                   {},
	"List-Id":                {},
	"List-Unsubscribe":       {},
	"Authentication-Results": {},
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

	result := Message{Headers: make(map[string][]string)}
	fields := reader.Header.Fields()
	for fields.Next() {
		key := textproto.CanonicalMIMEHeaderKey(fields.Key())
		if _, ok := selectedHeaders[key]; !ok {
			continue
		}
		value, err := fields.Text()
		if err != nil {
			return Message{}, fmt.Errorf("decode %s header: %w", key, err)
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
