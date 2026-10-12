package main

import (
	"bufio"
	"bytes"
	"context"
	"log"
	"net"
	"net/textproto"
	"strings"
	"time"

	messagetext "github.com/emersion/go-message/textproto"
	"github.com/emersion/go-milter"
	"typemail"
	"typemail/internal/cli"
)

const (
	maxMessageBytes      = 1 << 20
	classificationBudget = 4 * time.Second
	markerHeader         = "X-Typemail-Class"
)

type classifyFunc func(context.Context, typemail.Message) (typemail.Result, error)

type filter struct {
	milter.NoOpMilter
	ctx           context.Context
	classify      classifyFunc
	slots         chan struct{}
	networks      []*net.IPNet
	trusted       bool
	authenticated bool
	skipped       bool
	raw           bytes.Buffer
	headerEnd     int
	subjects      int
	markers       int
}

func newServer(ctx context.Context, classify classifyFunc, networks []*net.IPNet) *milter.Server {
	slots := make(chan struct{}, cli.DefaultConcurrency)
	return &milter.Server{
		Actions: milter.OptAddHeader | milter.OptChangeHeader,
		NewMilter: func() milter.Milter {
			// Unknown connection identity must never be treated as untrusted incoming mail.
			return &filter{ctx: ctx, classify: classify, slots: slots, networks: networks, trusted: true}
		},
	}
}

func (f *filter) reset() {
	f.authenticated, f.skipped = false, false
	f.raw = bytes.Buffer{}
	f.headerEnd, f.subjects, f.markers = 0, 0, 0
}

func (f *filter) Connect(_ string, family string, _ uint16, addr net.IP, _ *milter.Modifier) (milter.Response, error) {
	f.trusted = addr == nil || (family != "tcp4" && family != "tcp6")
	for _, network := range f.networks {
		if network.Contains(addr) {
			f.trusted = true
		}
	}
	return milter.RespContinue, nil
}

func authenticated(m *milter.Modifier) bool {
	if m == nil {
		return false
	}
	for _, key := range []string{"{auth_type}", "{auth_authen}", "auth_type", "auth_authen"} {
		if m.Macros[key] != "" {
			return true
		}
	}
	return false
}

func (f *filter) MailFrom(_ string, m *milter.Modifier) (milter.Response, error) {
	f.reset()
	f.authenticated = authenticated(m)
	return milter.RespContinue, nil
}

func (f *filter) skip(reason string) {
	if !f.skipped {
		log.Printf("typemail: pass through: %s", reason)
	}
	f.skipped = true
	f.raw = bytes.Buffer{}
}

func (f *filter) collect(parts ...[]byte) {
	if f.trusted || f.authenticated || f.skipped {
		return
	}
	size := f.raw.Len()
	for _, part := range parts {
		size += len(part)
	}
	if size > maxMessageBytes {
		f.skip("message exceeds 1 MiB")
		return
	}
	for _, part := range parts {
		f.raw.Write(part)
	}
}

// Header values may contain folds, but never a second header or NUL.
func validHeader(name, value string) bool {
	if name == "" || strings.ContainsAny(value, "\x00\r") {
		return false
	}
	for i := range name {
		if name[i] < 33 || name[i] > 126 || name[i] == ':' {
			return false
		}
	}
	for i := range value {
		if value[i] == '\n' && (i+1 == len(value) || (value[i+1] != ' ' && value[i+1] != '\t')) {
			return false
		}
	}
	return true
}

func (f *filter) Header(name, value string, m *milter.Modifier) (milter.Response, error) {
	// The library accumulates headers independently, even for skipped messages.
	// We keep our own bounded copy and never use its header map.
	if m != nil {
		defer clear(m.Headers)
	}
	if f.trusted || f.authenticated || f.skipped {
		return milter.RespContinue, nil
	}
	value = strings.ReplaceAll(value, "\r\n", "\n")
	if !validHeader(name, value) {
		f.skip("malformed header")
		return milter.RespContinue, nil
	}
	switch {
	case strings.EqualFold(name, "Subject"):
		f.subjects++
	case strings.EqualFold(name, markerHeader):
		f.markers++
	}
	if f.subjects > 1 || f.markers > 1 {
		f.skip("duplicate subject or class marker")
		return milter.RespContinue, nil
	}
	f.collect([]byte(name), []byte(": "), []byte(strings.ReplaceAll(value, "\n", "\r\n")), []byte("\r\n"))
	return milter.RespContinue, nil
}

func (f *filter) Headers(_ textproto.MIMEHeader, _ *milter.Modifier) (milter.Response, error) {
	f.collect([]byte("\r\n"))
	f.headerEnd = f.raw.Len()
	return milter.RespContinue, nil
}

func (f *filter) BodyChunk(chunk []byte, _ *milter.Modifier) (milter.Response, error) {
	f.collect(chunk)
	return milter.RespContinue, nil
}

type headerWriter interface {
	AddHeader(string, string) error
	ChangeHeader(int, string, string) error
}

func (f *filter) finish(w headerWriter) {
	if f.trusted || f.authenticated || f.skipped || f.ctx.Err() != nil {
		return
	}
	select {
	case f.slots <- struct{}{}:
		defer func() { <-f.slots }()
	default:
		f.skip("classification slots full")
		return
	}
	ctx, cancel := context.WithTimeout(f.ctx, classificationBudget)
	defer cancel()
	message, err := typemail.ParseMessage(bytes.NewReader(f.raw.Bytes()), cli.DefaultMaxEmailBytes)
	if err != nil {
		f.skip("message parsing failed")
		return
	}
	result, err := f.classify(ctx, message)
	if err != nil || ctx.Err() != nil {
		f.skip("classification failed or timed out")
		return
	}
	if result.Skipped {
		return
	}
	// Render everything before the first modification. The MIME body is never rewritten.
	var rewritten bytes.Buffer
	_, changed, err := typemail.RewriteSubject(&rewritten, bytes.NewReader(f.raw.Bytes()[:f.headerEnd]), result.Class)
	if err != nil {
		f.skip("subject rendering failed")
		return
	}
	if !changed {
		return
	}
	headers, err := messagetext.ReadHeader(bufio.NewReader(&rewritten))
	if err != nil {
		f.skip("rendered header parsing failed")
		return
	}
	if ctx.Err() != nil {
		f.skip("classification cancelled")
		return
	}
	// Once modifications begin, attempt both updates even if cancellation arrives.
	for _, update := range []struct {
		name, value string
		count       int
	}{
		{"Subject", headers.Get("Subject"), f.subjects},
		{markerHeader, headers.Get(markerHeader), f.markers},
	} {
		if update.count == 0 {
			err = w.AddHeader(update.name, update.value)
		} else {
			err = w.ChangeHeader(1, update.name, update.value)
		}
		if err != nil {
			f.skip("header modification failed; tag may be partial")
			return
		}
	}
}

func (f *filter) Body(m *milter.Modifier) (milter.Response, error) {
	defer f.reset()
	defer clear(m.Headers)
	f.authenticated = f.authenticated || authenticated(m)
	f.finish(m)
	// Accept recreates the backend and loses Connect identity. Continue at EOM delivers.
	return milter.RespContinue, nil
}

func (f *filter) Abort(_ *milter.Modifier) error { f.reset(); return nil }
