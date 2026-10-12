package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-milter"
	"typemail"
	"typemail/internal/cli"
)

// These helpers speak protocol 2 directly, including its terminal Continue at EOM.
type wireClient struct {
	t *testing.T
	net.Conn
}
type wireUpdate struct {
	code        byte
	index       uint32
	name, value string
}

func startWireServer(t *testing.T, ctx context.Context, classify classifyFunc, networks []*net.IPNet) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- newServer(ctx, classify, networks).Serve(ln) }()
	t.Cleanup(func() {
		// Server.Close races with Serve in go-milter v0.4.1.
		_ = ln.Close()
		select {
		case err := <-done:
			if !errors.Is(err, net.ErrClosed) {
				t.Errorf("Serve shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not stop")
		}
	})
	return ln.Addr().String()
}

func dialWire(t *testing.T, addr string) *wireClient {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c := &wireClient{t, conn}
	t.Cleanup(func() { _ = conn.Close() })
	var options bytes.Buffer
	for _, n := range []uint32{2, uint32(milter.OptAddHeader | milter.OptChangeHeader), 0} {
		_ = binary.Write(&options, binary.BigEndian, n)
	}
	c.send('O', options.Bytes())
	code, data := c.read()
	if code != 'O' || len(data) != 12 {
		t.Fatalf("negotiation: %c %x", code, data)
	}
	if version := binary.BigEndian.Uint32(data); version != 2 {
		t.Fatalf("version = %d", version)
	}
	if actions := binary.BigEndian.Uint32(data[4:]); actions != uint32(milter.OptAddHeader|milter.OptChangeHeader) {
		t.Fatalf("actions = %#x", actions)
	}
	if protocol := binary.BigEndian.Uint32(data[8:]); protocol != 0 {
		t.Fatalf("protocol flags = %#x", protocol)
	}
	return c
}

func (c *wireClient) send(code byte, data []byte) {
	c.t.Helper()
	_ = c.SetDeadline(time.Now().Add(6 * time.Second))
	var packet bytes.Buffer
	_ = binary.Write(&packet, binary.BigEndian, uint32(len(data)+1))
	packet.WriteByte(code)
	packet.Write(data)
	if _, err := io.Copy(c.Conn, &packet); err != nil {
		c.t.Fatal(err)
	}
}
func (c *wireClient) read() (byte, []byte) {
	c.t.Helper()
	var size uint32
	if err := binary.Read(c.Conn, binary.BigEndian, &size); err != nil {
		c.t.Fatal(err)
	}
	if size < 1 || size > 2<<20 {
		c.t.Fatalf("invalid packet size %d", size)
	}
	packet := make([]byte, size)
	if _, err := io.ReadFull(c.Conn, packet); err != nil {
		c.t.Fatal(err)
	}
	return packet[0], packet[1:]
}
func (c *wireClient) command(code byte, data []byte) {
	c.t.Helper()
	c.send(code, data)
	reply, payload := c.read()
	if reply != 'c' || len(payload) != 0 {
		c.t.Fatalf("command %c: want Continue, got %c %q", code, reply, payload)
	}
}
func (c *wireClient) connect(family byte, ip string) {
	data := append([]byte("remote.example\x00"), family)
	if family == '4' || family == '6' {
		data = append(data, 0, 25)
	}
	c.command('C', append(data, []byte(ip+"\x00")...))
	c.command('H', []byte("remote.example\x00"))
}
func (c *wireClient) macro(stage byte, name, value string) {
	c.send('D', []byte(string(stage)+name+"\x00"+value+"\x00"))
}
func (c *wireClient) message(headers [][2]string, body string) {
	c.command('M', []byte("<sender@example.org>\x00"))
	c.command('R', []byte("<recipient@example.org>\x00"))
	for _, h := range headers {
		c.command('L', []byte(h[0]+"\x00"+h[1]+"\x00"))
	}
	c.command('N', nil)
	for len(body) > 0 {
		n := min(len(body), milter.MaxBodyChunk)
		c.command('B', []byte(body[:n]))
		body = body[n:]
	}
}
func (c *wireClient) finish() []wireUpdate {
	c.t.Helper()
	c.send('E', nil)
	var updates []wireUpdate
	for {
		code, data := c.read()
		if code == 'c' {
			if len(data) != 0 {
				c.t.Fatalf("Continue payload: %q", data)
			}
			return updates
		}
		update := wireUpdate{code: code}
		switch code {
		case 'm':
			if len(data) < 4 {
				c.t.Fatal("short change-header packet")
			}
			update.index = binary.BigEndian.Uint32(data)
			data = data[4:]
		case 'h':
		default:
			c.t.Fatalf("unexpected EOM response %c %q, only header updates and Continue allowed", code, data)
		}
		fields := strings.Split(string(data), "\x00")
		if len(fields) != 3 || fields[2] != "" {
			c.t.Fatalf("header packet: %q", data)
		}
		update.name, update.value = fields[0], fields[1]
		updates = append(updates, update)
	}
}
func checkUpdates(t *testing.T, got []wireUpdate, subject, class string, subjectCode, markerCode byte) {
	t.Helper()
	decoded, err := new(mime.WordDecoder).DecodeHeader(gotSubject(got))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || decoded != subject {
		t.Fatalf("updates = %#v, decoded subject %q; want %q", got, decoded, subject)
	}
	for i, expected := range []wireUpdate{{code: subjectCode, name: "Subject"}, {code: markerCode, name: markerHeader, value: class}} {
		wantIndex := uint32(0)
		if expected.code == 'm' {
			wantIndex = 1
		}
		if got[i].code != expected.code || got[i].index != wantIndex || got[i].name != expected.name || (i == 1 && got[i].value != class) {
			t.Fatalf("update %d = %#v", i, got[i])
		}
	}
}
func gotSubject(updates []wireUpdate) string {
	if len(updates) == 0 {
		return ""
	}
	return updates[0].value
}

func TestWireClassification(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		headers                     [][2]string
		class                       typemail.Class
		inputSubject, outputSubject string
		subjectCode, markerCode     byte
		unchanged                   bool
	}{
		{"default", [][2]string{{"Subject", "Hello"}}, typemail.ClassLegitimate, "Hello", "[legitimate] Hello", 'm', 'h', false},
		{"spam", [][2]string{{"Subject", "Offer"}}, typemail.ClassSpam, "Offer", "[spam] Offer", 'm', 'h', false},
		{"custom", [][2]string{{"Subject", "Release"}}, "project updates", "Release", "[project updates] Release", 'm', 'h', false},
		{"encoded", [][2]string{{"Subject", "=?UTF-8?B?Y2Fmw6k=?="}}, typemail.ClassNewsletter, "café", "[newsletter] café", 'm', 'h', false},
		{"missing", nil, typemail.ClassPhishing, "", "[phishing]", 'h', 'h', false},
		{"retag", [][2]string{{"Subject", "[spam] Hello"}, {markerHeader, "spam"}}, typemail.ClassLegitimate, "Hello", "[legitimate] Hello", 'm', 'm', false},
		{"already tagged", [][2]string{{"Subject", "[spam] Hello"}, {markerHeader, "spam"}}, typemail.ClassSpam, "Hello", "", 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			c := dialWire(t, startWireServer(t, context.Background(), func(ctx context.Context, message typemail.Message) (typemail.Result, error) {
				calls.Add(1)
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 4*time.Second {
					t.Error("classification deadline must be <=4 seconds")
				}
				if got := strings.Join(message.Headers["Subject"], ""); got != tc.inputSubject {
					t.Errorf("classifier subject = %q", got)
				}
				return typemail.Result{Class: tc.class}, nil
			}, nil))
			c.connect('4', "203.0.113.5")
			c.message(tc.headers, "Hello body")
			updates := c.finish()
			if tc.unchanged {
				if len(updates) != 0 {
					t.Fatalf("updates = %#v", updates)
				}
			} else {
				checkUpdates(t, updates, tc.outputSubject, string(tc.class), tc.subjectCode, tc.markerCode)
			}
			if calls.Load() != 1 {
				t.Fatalf("classifications = %d", calls.Load())
			}
		})
	}
}

func TestWireMultipartTextLimit(t *testing.T) {
	plain := strings.Repeat("p", 10<<10)
	html := strings.Repeat("h", 10<<10)
	body := "--part\r\nContent-Type: text/plain\r\n\r\n" + plain + "\r\n--part\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=secret.txt\r\n\r\nATTACHMENT SECRET\r\n--part\r\nContent-Type: text/html\r\n\r\n" + html + "\r\n--part--\r\n"
	var calls atomic.Int32
	c := dialWire(t, startWireServer(t, context.Background(), func(_ context.Context, message typemail.Message) (typemail.Result, error) {
		calls.Add(1)
		want := []typemail.TextPart{{ContentType: "text/plain", Text: plain}, {ContentType: "text/html", Text: html[:6<<10]}}
		if !reflect.DeepEqual(message.TextParts, want) {
			t.Errorf("inline text differs from the expected attachment-free 16KiB content")
		}
		return typemail.Result{Class: typemail.ClassSpam}, nil
	}, nil))
	c.connect('4', "203.0.113.5")
	c.message([][2]string{{"Subject", "Multipart"}, {"MIME-Version", "1.0"}, {"Content-Type", "multipart/mixed; boundary=part"}}, body)
	checkUpdates(t, c.finish(), "[spam] Multipart", "spam", 'm', 'h')
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestWireBypass(t *testing.T) {
	networks, err := parseNetworks("127.0.0.0/8, [::1]/128")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		family    byte
		ip, macro string
		stage     byte
	}{
		{"trusted ipv4", '4', "127.0.0.2", "", 0}, {"trusted ipv6", '6', "::1", "", 0},
		{"unknown", 'U', "", "", 0}, {"unix", 'L', "/socket", "", 0}, {"invalid ip", '4', "not-an-ip", "", 0}, {"no connect", 0, "", "", 0},
		{"auth type", '4', "203.0.113.5", "{auth_type}", 'M'}, {"auth user", '4', "203.0.113.5", "{auth_authen}", 'M'},
		{"unbraced type", '4', "203.0.113.5", "auth_type", 'M'}, {"unbraced user", '4', "203.0.113.5", "auth_authen", 'M'},
		{"auth at EOM", '4', "203.0.113.5", "{auth_authen}", 'E'},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			c := dialWire(t, startWireServer(t, context.Background(), func(context.Context, typemail.Message) (typemail.Result, error) {
				calls.Add(1)
				return typemail.Result{Class: typemail.ClassSpam}, nil
			}, networks))
			if tc.family != 0 {
				c.connect(tc.family, tc.ip)
			}
			if tc.stage == 'M' {
				c.macro('M', tc.macro, "authenticated")
			}
			c.message([][2]string{{"Subject", "Hello"}}, "Body")
			if tc.stage == 'E' {
				c.macro('E', tc.macro, "authenticated")
			}
			if updates := c.finish(); len(updates) != 0 {
				t.Fatalf("bypass modified headers: %#v", updates)
			}
			if calls.Load() != 0 {
				t.Fatal("bypass classified mail")
			}
		})
	}
}

func TestWireRepeatedTransactionsAndAbort(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		t.Run(map[bool]string{false: "incoming", true: "trusted"}[trusted], func(t *testing.T) {
			var calls atomic.Int32
			networks, _ := parseNetworks("127.0.0.0/8")
			c := dialWire(t, startWireServer(t, context.Background(), func(_ context.Context, message typemail.Message) (typemail.Result, error) {
				calls.Add(1)
				if !reflect.DeepEqual(message.Headers["Subject"], []string{"Fresh"}) {
					t.Errorf("stale headers: %#v", message.Headers)
				}
				if !reflect.DeepEqual(message.TextParts, []typemail.TextPart{{ContentType: "text/plain", Text: "Body"}}) {
					t.Error("stale body from previous transaction")
				}
				return typemail.Result{Class: typemail.ClassSpam}, nil
			}, networks))
			ip := "203.0.113.5"
			if trusted {
				ip = "127.0.0.2"
			}
			c.connect('4', ip)
			for i := 0; i < 2; i++ {
				c.message([][2]string{{"Subject", "Fresh"}}, "Body")
				updates := c.finish()
				if trusted {
					if len(updates) != 0 {
						t.Fatal("trusted mail modified")
					}
				} else {
					checkUpdates(t, updates, "[spam] Fresh", "spam", 'm', 'h')
				}
				if i == 0 {
					c.macro('M', "{auth_authen}", "user")
					c.message([][2]string{{"Subject", "Authenticated"}}, "Private")
					if updates := c.finish(); len(updates) != 0 {
						t.Fatal("authenticated transaction modified")
					}
					// The MTA supplies fresh transaction macros on the same connection.
					c.send('D', []byte{'M'})
				}
			}
			c.macro('M', "{auth_authen}", "user")
			c.message([][2]string{{"Subject", "Aborted"}, {"Subject", "Duplicate"}}, "Discard")
			c.send('A', nil) // Abort has no reply. The following transaction also detects stray replies.
			c.message([][2]string{{"Subject", "Fresh"}}, "Body")
			updates := c.finish()
			if trusted {
				if len(updates) != 0 {
					t.Fatal("abort lost trusted identity")
				}
			} else {
				checkUpdates(t, updates, "[spam] Fresh", "spam", 'm', 'h')
			}
			want := int32(3)
			if trusted {
				want = 0
			}
			if calls.Load() != want {
				t.Fatalf("calls = %d, want %d", calls.Load(), want)
			}
		})
	}
}

func TestWireFailOpen(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers [][2]string
		body    string
		class   typemail.Class
		err     error
		calls   int32
	}{
		{"duplicate subject", [][2]string{{"Subject", "One"}, {"sUbJeCt", "Two"}}, "Body", "spam", nil, 0},
		{"duplicate marker", [][2]string{{markerHeader, "spam"}, {"x-typemail-class", "spam"}}, "Body", "spam", nil, 0},
		{"header injection", [][2]string{{"Subject", "One\nInjected: value"}}, "Body", "spam", nil, 0},
		{"invalid header name", [][2]string{{"Bad:Name", "value"}}, "Body", "spam", nil, 0},
		{"malformed MIME", [][2]string{{"Content-Type", "multipart/mixed"}}, "Body", "spam", nil, 0},
		{"malformed encoding", [][2]string{{"Content-Transfer-Encoding", "base64"}}, "!!!", "spam", nil, 0},
		{"render invalid class", [][2]string{{"Subject", "Hello"}}, "Body", "bad\nclass", nil, 1},
		{"over 1MiB", [][2]string{{"Subject", "Hello"}}, strings.Repeat("x", (1<<20)+1), "spam", nil, 0},
		{"provider failure", [][2]string{{"Subject", "Hello"}}, "Body", "spam", errors.New("provider unavailable"), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			c := dialWire(t, startWireServer(t, context.Background(), func(context.Context, typemail.Message) (typemail.Result, error) {
				calls.Add(1)
				return typemail.Result{Class: tc.class}, tc.err
			}, nil))
			c.connect('4', "203.0.113.5")
			c.message(tc.headers, tc.body)
			if updates := c.finish(); len(updates) != 0 {
				t.Fatalf("fail-open updates: %#v", updates)
			}
			if calls.Load() != tc.calls {
				t.Fatalf("calls = %d, want %d", calls.Load(), tc.calls)
			}
		})
	}
}

func TestWireCancellation(t *testing.T) {
	for _, mode := range []string{"already cancelled", "during classification", "parent deadline"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			if mode == "parent deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
			}
			defer cancel()
			if mode == "already cancelled" {
				cancel()
			}
			var calls atomic.Int32
			c := dialWire(t, startWireServer(t, ctx, func(ctx context.Context, _ typemail.Message) (typemail.Result, error) {
				calls.Add(1)
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 4*time.Second {
					t.Error("missing bounded deadline")
				}
				if mode == "during classification" {
					cancel()
				}
				<-ctx.Done()
				// Even an apparently successful result after cancellation must not modify mail.
				return typemail.Result{Class: typemail.ClassSpam}, nil
			}, nil))
			c.connect('4', "203.0.113.5")
			c.message([][2]string{{"Subject", "Hello"}}, "Body")
			if updates := c.finish(); len(updates) != 0 {
				t.Fatalf("cancelled updates: %#v", updates)
			}
			want := int32(1)
			if mode == "already cancelled" {
				want = 0
			}
			if calls.Load() != want {
				t.Fatalf("calls = %d", calls.Load())
			}
		})
	}
}

func TestWireSaturation(t *testing.T) {
	entered := make(chan struct{}, cli.DefaultConcurrency)
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	addr := startWireServer(t, ctx, func(ctx context.Context, _ typemail.Message) (typemail.Result, error) {
		calls.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
			return typemail.Result{Class: typemail.ClassSpam}, nil
		case <-ctx.Done():
			return typemail.Result{}, ctx.Err()
		}
	}, nil)
	var busy []*wireClient
	for i := 0; i < cli.DefaultConcurrency; i++ {
		c := dialWire(t, addr)
		c.connect('4', "203.0.113.5")
		c.message([][2]string{{"Subject", "Busy"}}, "Body")
		c.send('E', nil)
		busy = append(busy, c)
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("classification did not start")
		}
	}
	extra := dialWire(t, addr)
	extra.connect('4', "203.0.113.5")
	extra.message([][2]string{{"Subject", "Overflow"}}, "Body")
	if updates := extra.finish(); len(updates) != 0 {
		t.Fatalf("saturated updates: %#v", updates)
	}
	if calls.Load() != int32(cli.DefaultConcurrency) {
		t.Fatal("saturated message reached provider")
	}
	close(release)
	for _, c := range busy {
		// EOM was already sent; read its two updates and final Continue.
		for _, want := range []byte{'m', 'h', 'c'} {
			code, _ := c.read()
			if code != want {
				t.Fatalf("busy response = %c, want %c", code, want)
			}
		}
	}
	extra.message([][2]string{{"Subject", "Recovered"}}, "Body")
	checkUpdates(t, extra.finish(), "[spam] Recovered", "spam", 'm', 'h')
}

type failingHeaderWriter struct{ failAfter, attempts, updates int }

func (w *failingHeaderWriter) AddHeader(string, string) error {
	w.attempts++
	if w.updates == w.failAfter {
		return errors.New("write failed")
	}
	w.updates++
	return nil
}
func (w *failingHeaderWriter) ChangeHeader(_ int, name, value string) error {
	return w.AddHeader(name, value)
}

func TestHeaderWriteFailureResetsAfterBody(t *testing.T) {
	for _, after := range []int{0, 1} {
		t.Run(map[int]string{0: "before first update", 1: "after subject update"}[after], func(t *testing.T) {
			f := &filter{ctx: context.Background(), slots: make(chan struct{}, 1), classify: func(context.Context, typemail.Message) (typemail.Result, error) {
				return typemail.Result{Class: typemail.ClassSpam}, nil
			}}
			_, _ = f.MailFrom("sender", nil)
			_, _ = f.Header("Subject", "Hello", nil)
			_, _ = f.Headers(nil, nil)
			_, _ = f.BodyChunk([]byte("Body"), nil)
			writer := &failingHeaderWriter{failAfter: after}
			f.finish(writer)
			if writer.updates != after || writer.attempts != after+1 || !f.skipped {
				t.Fatalf("writer = %#v, skipped = %v", writer, f.skipped)
			}
			headers := textproto.MIMEHeader{"Subject": {"Hello"}}
			response, err := f.Body(&milter.Modifier{Headers: headers})
			if err != nil || response != milter.RespContinue {
				t.Fatalf("Body = %v, %v; want Continue", response, err)
			}
			if f.skipped || f.authenticated || f.raw.Len() != 0 || f.subjects != 0 || f.markers != 0 || f.headerEnd != 0 || len(headers) != 0 || len(f.slots) != 0 {
				t.Fatal("Body did not reset transaction state")
			}
		})
	}
}

// The classifier's client uses http.DefaultTransport. Redirect only this test's
// provider requests to localhost without exposing classifier internals.
type outageTransport struct {
	target string
	base   http.RoundTripper
}

func (tr outageTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	url := *req.URL
	clone.URL = &url
	clone.URL.Scheme, clone.URL.Host = "http", tr.target
	return tr.base.RoundTrip(clone)
}
func TestWireHTTPProviderOutage(t *testing.T) {
	var requests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer provider.Close()
	previous := http.DefaultTransport
	http.DefaultTransport = outageTransport{target: strings.TrimPrefix(provider.URL, "http://"), base: previous}
	defer func() { http.DefaultTransport = previous }()
	classifier, err := typemail.NewClassifier("test-key")
	if err != nil {
		t.Fatal(err)
	}
	c := dialWire(t, startWireServer(t, context.Background(), classifier.Classify, nil))
	c.connect('4', "203.0.113.5")
	c.message([][2]string{{"Subject", "Hello"}}, "Body")
	if updates := c.finish(); len(updates) != 0 {
		t.Fatalf("outage updates: %#v", updates)
	}
	if requests.Load() != 3 {
		t.Fatalf("provider attempts = %d, want 3", requests.Load())
	}
}

func TestWireClassificationBudget(t *testing.T) {
	c := dialWire(t, startWireServer(t, context.Background(), func(ctx context.Context, _ typemail.Message) (typemail.Result, error) {
		<-ctx.Done()
		return typemail.Result{}, ctx.Err()
	}, nil))
	c.connect('4', "203.0.113.5")
	c.message([][2]string{{"Subject", "Hello"}}, "Body")
	start := time.Now()
	if updates := c.finish(); len(updates) != 0 {
		t.Fatalf("timeout modified headers: %#v", updates)
	}
	if elapsed := time.Since(start); elapsed < classificationBudget || elapsed > classificationBudget+time.Second {
		t.Fatalf("classification took %v, expected four-second budget", elapsed)
	}
}

type cancellingWriter struct {
	cancel  context.CancelFunc
	updates int
}

func (w *cancellingWriter) AddHeader(string, string) error {
	w.updates++
	w.cancel()
	return nil
}
func (w *cancellingWriter) ChangeHeader(_ int, name, value string) error {
	return w.AddHeader(name, value)
}

func TestCancellationAfterFirstWriteCompletesTag(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &filter{ctx: ctx, slots: make(chan struct{}, 1), classify: func(context.Context, typemail.Message) (typemail.Result, error) {
		return typemail.Result{Class: typemail.ClassSpam}, nil
	}}
	_, _ = f.MailFrom("sender", nil)
	_, _ = f.Header("Subject", "Hello", nil)
	_, _ = f.Headers(nil, nil)
	_, _ = f.BodyChunk([]byte("Body"), nil)
	w := &cancellingWriter{cancel: cancel}
	f.finish(w)
	if w.updates != 2 {
		t.Fatalf("updates = %d, want complete tag", w.updates)
	}
}

func TestLibraryHeaderCacheIsCleared(t *testing.T) {
	for _, skipped := range []bool{false, true} {
		f := &filter{skipped: skipped}
		h := textproto.MIMEHeader{"Subject": {"Hello"}}
		_, _ = f.Header("Subject", "Hello", &milter.Modifier{Headers: h})
		if len(h) != 0 {
			t.Fatal("unbounded library header cache retained")
		}
	}
}
