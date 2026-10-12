package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/mail"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"typemail/pkg/typemail"
)

// Each script ends with a NOOP barrier. Unexpected mutations therefore fail
// even when the operation under test returns early.
type rewriteWire struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

func (w *rewriteWire) send(format string, args ...any) {
	w.t.Helper()
	if _, err := fmt.Fprintf(w.conn, format, args...); err != nil {
		w.t.Errorf("script write: %v", err)
	}
}

func (w *rewriteWire) command(pattern string) (string, string) {
	w.t.Helper()
	line, err := w.r.ReadString('\n')
	if err != nil {
		w.t.Errorf("read command matching %q: %v", pattern, err)
		return "", ""
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	tag, command, ok := strings.Cut(line, " ")
	if !ok || !regexp.MustCompile("^(?:"+pattern+")$").MatchString(command) {
		w.t.Errorf("wire command = %q, want %q", line, pattern)
		w.conn.Close()
		return "", ""
	}
	return tag, command
}

func scriptedRewriteClient(t *testing.T, caps string, script func(*rewriteWire)) *imapclient.Client {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	clientConn.SetDeadline(deadline)
	serverConn.SetDeadline(deadline)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		wire := &rewriteWire{t: t, conn: serverConn, r: bufio.NewReader(serverConn)}
		wire.send("* PREAUTH [CAPABILITY %s] ready\r\n", caps)
		script(wire)
		tag, _ := wire.command("NOOP")
		wire.send("%s OK done\r\n", tag)
	}()
	client := imapclient.New(clientConn, nil)
	t.Cleanup(func() {
		client.Close()
		serverConn.Close()
		<-done
	})
	if err := client.WaitGreeting(); err != nil {
		t.Fatal(err)
	}
	return client
}

func rewriteBarrier(t *testing.T, client *imapclient.Client) {
	t.Helper()
	if err := client.Noop().Wait(); err != nil {
		t.Fatalf("connection not usable after operation: %v", err)
	}
}

func rewriteTempDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Cleanup(func() {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Errorf("rewrite temporary files not cleaned: %v, %v", entries, err)
		}
	})
}

func TestPrepareMailboxProtocol(t *testing.T) {
	for _, tc := range []struct {
		name, caps, selection string
		rewrite               bool
		unsupported           bool
	}{
		{"default is read only", "IMAP4rev1", "EXAMINE", false, false},
		{"UIDPLUS permits rewrite", "IMAP4rev1 UIDPLUS", "SELECT", true, false},
		{"rev2 permits rewrite", "IMAP4rev2", "SELECT", true, false},
		{"unsupported does not select", "IMAP4rev1", "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := scriptedRewriteClient(t, tc.caps, func(w *rewriteWire) {
				if tc.unsupported {
					return
				}
				tag, _ := w.command(tc.selection + ` INBOX`)
				w.send("* 3 EXISTS\r\n* OK [UIDVALIDITY 9] valid\r\n%s OK selected\r\n", tag)
				tag, _ = w.command(`UID SEARCH(?: RETURN \([^)]*\))? (?:NOT DELETED|UNDELETED)`)
				w.send("* SEARCH 7 42\r\n%s OK searched\r\n", tag)
			})
			uids, validity, err := prepareMailbox(client, tc.rewrite)
			if tc.unsupported {
				if err == nil || !strings.Contains(err.Error(), "UIDPLUS") {
					t.Fatalf("error = %v", err)
				}
			} else if err != nil || validity != 9 || !reflect.DeepEqual(uids, []imap.UID{7, 42}) {
				t.Fatalf("snapshot = %v, %v", uids, err)
			}
			rewriteBarrier(t, client)
		})
	}
}

func TestPrepareMailboxRequiresSelectedUIDValidity(t *testing.T) {
	client := scriptedRewriteClient(t, "IMAP4rev1 UIDPLUS", func(w *rewriteWire) {
		tag, _ := w.command(`SELECT INBOX`)
		w.send("* 3 EXISTS\r\n%s OK selected\r\n", tag)
	})
	uids, validity, err := prepareMailbox(client, true)
	if err == nil || !strings.Contains(err.Error(), "UIDVALIDITY") || len(uids) != 0 || validity != 0 {
		t.Fatalf("snapshot = %v, %d, %v", uids, validity, err)
	}
	rewriteBarrier(t, client)
}

const rewriteOriginal = "From: sender@example.org\r\nSubject: hello\r\nContent-Type: multipart/mixed; boundary=example\r\nX-Keep: untouched\r\n\r\n--example\r\nContent-Type: text/plain\r\n\r\nhello\r\n--example\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n\r\nAAECAw==\r\n--example--\r\n"
const rewriteMetadata = `UID 42 FLAGS (\Seen \Answered custom \Recent) INTERNALDATE "02-Jan-2024 03:04:05 +0200"`

func rewriteInput() classificationResult {
	return classificationResult{uid: 42, from: "sender@example.org", subject: "hello", result: typemail.Result{Class: typemail.Class("selling"), Confidence: 0.9}}
}

func (w *rewriteWire) fetch(metadata, body string) {
	w.t.Helper()
	tag, command := w.command(`UID FETCH 42 \(.*\)`)
	for _, item := range []string{"UID", "FLAGS", "INTERNALDATE", "BODY.PEEK[]"} {
		if !strings.Contains(command, item) {
			w.t.Errorf("fetch missing %s: %s", item, command)
		}
	}
	w.send("* 1 FETCH (%s BODY[] {%d}\r\n%s)\r\n%s OK fetched\r\n", metadata, len(body), body, tag)
}

func (w *rewriteWire) append(status string, beforeReply ...func()) {
	w.t.Helper()
	tag, command := w.command(`APPEND INBOX \(\\Seen \\Answered custom\) "(?:02| 2)-Jan-2024 03:04:05 \+0200" \{[0-9]+\}`)
	match := regexp.MustCompile(`\{([0-9]+)\}$`).FindStringSubmatch(command)
	if len(match) != 2 {
		return
	}
	size, _ := strconv.Atoi(match[1])
	w.send("+ upload\r\n")
	body := make([]byte, size)
	if _, err := io.ReadFull(w.r, body); err != nil {
		w.t.Errorf("append literal: %v", err)
		return
	}
	if line, err := w.r.ReadString('\n'); err != nil || line != "\r\n" {
		w.t.Errorf("literal terminator = %q, %v", line, err)
	}
	message, err := mail.ReadMessage(strings.NewReader(string(body)))
	if err != nil {
		w.t.Errorf("replacement message: %v", err)
		return
	}
	if message.Header.Get("Subject") != "[selling] hello" || message.Header.Get("X-Keep") != "untouched" || message.Header.Get("From") != "sender@example.org" {
		w.t.Errorf("replacement headers = %v", message.Header)
	}
	wantBody := strings.SplitN(rewriteOriginal, "\r\n\r\n", 2)[1]
	gotBody, _ := io.ReadAll(message.Body)
	if string(gotBody) != wantBody {
		w.t.Errorf("replacement MIME body changed: %q", gotBody)
	}
	for _, hook := range beforeReply {
		hook()
	}
	if status != "" {
		w.send("%s %s\r\n", tag, status)
	}
}

func TestRewriteMessageClassificationSkipDoesNotTouchFilesOrIMAP(t *testing.T) {
	// An unusable temporary directory makes even an attempted file creation fail.
	t.Setenv("TMPDIR", t.TempDir()+"/missing")
	client := scriptedRewriteClient(t, "IMAP4rev1 UIDPLUS", func(*rewriteWire) {})
	input := rewriteInput()
	input.result.Skipped = true
	result := rewriteMessage(context.Background(), client, 9, input)
	if !reflect.DeepEqual(result, input) {
		t.Fatalf("skipped result = %+v, want unchanged %+v", result, input)
	}
	rewriteBarrier(t, client)
}

func TestRewriteMessageReplacementProtocol(t *testing.T) {
	for _, tc := range []struct {
		name, appendStatus, storeStatus, expungeStatus, wantErr string
	}{
		{"success", "OK [APPENDUID 9 99] appended", "OK stored", "OK expunged", ""},
		{"missing append UID", "OK appended", "", "", "original untouched"},
		{"zero UID validity", "OK [APPENDUID 0 99] appended", "", "", "original untouched"},
		{"different UID validity", "OK [APPENDUID 10 99] appended", "", "", "original untouched"},
		{"replacement equals original", "OK [APPENDUID 9 42] appended", "", "", "original untouched"},
		{"rejected append", "NO quota exceeded", "", "", "original untouched"},
		{"store failure", "OK [APPENDUID 9 99] appended", "NO cannot store", "", "replacement UID 99 retained; original UID 42 cleanup failed"},
		{"expunge failure", "OK [APPENDUID 9 99] appended", "OK stored", "NO cannot expunge", "replacement UID 99 retained; original UID 42 cleanup failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rewriteTempDir(t)
			client := scriptedRewriteClient(t, "IMAP4rev1 UIDPLUS", func(w *rewriteWire) {
				w.fetch(strings.ReplaceAll(rewriteMetadata, `\Recent`, `\rEcEnT`), rewriteOriginal)
				w.append(tc.appendStatus)
				if tc.storeStatus == "" {
					return
				}
				tag, _ := w.command(`UID STORE 42 \+FLAGS.SILENT \(\\Deleted\)`)
				w.send("%s %s\r\n", tag, tc.storeStatus)
				if tc.expungeStatus == "" {
					return
				}
				tag, _ = w.command(`UID EXPUNGE 42`)
				w.send("%s %s\r\n", tag, tc.expungeStatus)
			})
			result := rewriteMessage(context.Background(), client, 9, rewriteInput())
			if tc.wantErr == "" {
				if result.err != nil || result.uid != 99 || result.subject != "[selling] hello" || result.from != rewriteInput().from || !reflect.DeepEqual(result.result, rewriteInput().result) {
					t.Fatalf("replacement result = %+v", result)
				}
			} else if result.err == nil || !strings.Contains(result.err.Error(), tc.wantErr) || result.uid != 42 {
				t.Fatalf("failure result = %+v, want %q with original UID", result, tc.wantErr)
			}
			rewriteBarrier(t, client)
		})
	}
}

func TestRewriteMessageSkipsAndInvalidMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, metadata, body, skip, wantErr string
		absent, nilBody                     bool
	}{
		{name: "disappeared", absent: true, skip: "message disappeared"},
		{name: "deleted", metadata: `UID 42 FLAGS (\Deleted) INTERNALDATE "02-Jan-2024 03:04:05 +0200"`, body: rewriteOriginal, skip: "message is deleted"},
		{name: "mixed case deleted", metadata: `UID 42 FLAGS (\dElEtEd) INTERNALDATE "02-Jan-2024 03:04:05 +0200"`, body: rewriteOriginal, skip: "message is deleted"},
		{name: "missing flags", metadata: `UID 42 INTERNALDATE "02-Jan-2024 03:04:05 +0200"`, body: rewriteOriginal, wantErr: "incomplete metadata"},
		{name: "missing date", metadata: `UID 42 FLAGS ()`, body: rewriteOriginal, wantErr: "incomplete metadata"},
		{name: "nil body", metadata: rewriteMetadata, nilBody: true, wantErr: "incomplete metadata"},
		{name: "unrelated UID is ignored", metadata: `UID 43 FLAGS () INTERNALDATE "02-Jan-2024 03:04:05 +0200"`, body: rewriteOriginal, skip: "message disappeared"},
		{name: "malformed drains literal", metadata: rewriteMetadata, body: "Invalid header\r\n\r\n" + strings.Repeat("unread body\r\n", 10000), wantErr: "rewrite subject"},
		{name: "already tagged", metadata: rewriteMetadata, body: "Subject: [selling] hello\r\nX-TypeMail-Class: selling\r\n\r\nbody", skip: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rewriteTempDir(t)
			client := scriptedRewriteClient(t, "IMAP4rev1 UIDPLUS", func(w *rewriteWire) {
				if tc.absent {
					tag, _ := w.command(`UID FETCH 42 \(.*\)`)
					w.send("%s OK gone\r\n", tag)
				} else if tc.nilBody {
					tag, _ := w.command(`UID FETCH 42 \(.*\)`)
					w.send("* 1 FETCH (%s BODY[] NIL)\r\n%s OK fetched\r\n", tc.metadata, tag)
				} else {
					w.fetch(tc.metadata, tc.body)
				}
			})
			result := rewriteMessage(context.Background(), client, 9, rewriteInput())
			if tc.wantErr != "" {
				if result.err == nil || !strings.Contains(result.err.Error(), tc.wantErr) {
					t.Fatalf("result = %+v, want %q", result, tc.wantErr)
				}
			} else if result.err != nil || result.skipped != tc.skip {
				t.Fatalf("result = %+v", result)
			}
			if result.uid != 42 {
				t.Fatalf("original UID changed: %+v", result)
			}
			rewriteBarrier(t, client)
		})
	}
}

func TestRewriteMessageCancellation(t *testing.T) {
	for _, midfetch := range []bool{false, true} {
		t.Run(fmt.Sprintf("midfetch=%v", midfetch), func(t *testing.T) {
			rewriteTempDir(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if !midfetch {
				cancel()
			}
			client := scriptedRewriteClient(t, "IMAP4rev1 UIDPLUS", func(w *rewriteWire) {
				if midfetch {
					tag, _ := w.command(`UID FETCH 42 \(.*\)`)
					cancel()
					w.send("* 1 FETCH (%s BODY[] {%d}\r\n%s)\r\n%s OK fetched\r\n", rewriteMetadata, len(rewriteOriginal), rewriteOriginal, tag)
				}
			})
			result := rewriteMessage(ctx, client, 9, rewriteInput())
			if !errors.Is(result.err, context.Canceled) || result.uid != 42 {
				t.Fatalf("canceled result = %+v", result)
			}
			rewriteBarrier(t, client)
		})
	}
}

func TestRewriteMessageAppendConnectionLoss(t *testing.T) {
	for _, interrupted := range []bool{true, false} {
		t.Run(fmt.Sprintf("interrupted=%v", interrupted), func(t *testing.T) {
			rewriteTempDir(t)
			clientConn, serverConn := net.Pipe()
			deadline := time.Now().Add(time.Second)
			clientConn.SetDeadline(deadline)
			serverConn.SetDeadline(deadline)
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer serverConn.Close()
				w := &rewriteWire{t: t, conn: serverConn, r: bufio.NewReader(serverConn)}
				w.send("* PREAUTH [CAPABILITY IMAP4rev1 UIDPLUS] ready\r\n")
				w.fetch(rewriteMetadata, rewriteOriginal)
				if interrupted {
					_, _ = w.command(`APPEND INBOX .* \{[0-9]+\}`)
					w.send("+ upload\r\n")
					if _, err := w.r.ReadByte(); err != nil {
						t.Errorf("read partial APPEND: %v", err)
					}
				} else {
					// The server accepted the literal but its confirmation is lost.
					w.append("")
				}
			}()
			client := imapclient.New(clientConn, nil)
			t.Cleanup(func() {
				client.Close()
				serverConn.Close()
				<-done
			})
			if err := client.WaitGreeting(); err != nil {
				t.Fatal(err)
			}
			result := rewriteMessage(context.Background(), client, 9, rewriteInput())
			if result.err == nil || result.uid != 42 {
				t.Fatalf("connection loss result = %+v", result)
			}
			for _, want := range []string{"ambiguous", "original untouched", "do not retry automatically"} {
				if !strings.Contains(result.err.Error(), want) {
					t.Errorf("error = %v, want %q", result.err, want)
				}
			}
			<-done
		})
	}
}

func TestRewriteMessageCancellationAfterConfirmedAppend(t *testing.T) {
	rewriteTempDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := scriptedRewriteClient(t, "IMAP4rev1 UIDPLUS", func(w *rewriteWire) {
		w.fetch(rewriteMetadata, rewriteOriginal)
		w.append("OK [APPENDUID 9 99] appended", cancel)
	})
	result := rewriteMessage(ctx, client, 9, rewriteInput())
	if !errors.Is(result.err, context.Canceled) || result.uid != 42 || !strings.Contains(result.err.Error(), "replacement UID 99 retained; original UID 42 cleanup failed") {
		t.Fatalf("canceled result = %+v", result)
	}
	// The barrier rejects STORE, EXPUNGE, or an automatic APPEND retry.
	rewriteBarrier(t, client)
}

func TestRewriteResultsForwardsExactlyOnce(t *testing.T) {
	rewriteTempDir(t)
	client := scriptedRewriteClient(t, "IMAP4rev1 UIDPLUS", func(w *rewriteWire) {
		tag, _ := w.command(`UID FETCH 42 \(.*\)`)
		w.send("%s OK disappeared\r\n", tag)
	})
	input := make(chan classificationResult, 2)
	failed := rewriteInput()
	failed.uid = 7
	failed.err = errors.New("classification failed")
	input <- failed
	input <- rewriteInput()
	close(input)
	var mutex sync.Mutex
	var results []classificationResult
	for result := range rewriteResults(context.Background(), client, 9, &mutex, input) {
		results = append(results, result)
	}
	if len(results) != 2 || !reflect.DeepEqual(results[0], failed) || results[1].uid != 42 || results[1].err != nil || results[1].skipped != "message disappeared" {
		t.Fatalf("forwarded results = %+v", results)
	}
	rewriteBarrier(t, client)
}

func TestRewriteResultsSerializesConcurrentFetch(t *testing.T) {
	rewriteTempDir(t)
	var mutex sync.Mutex
	fetchStarted := make(chan struct{})
	competingStarted := make(chan struct{})
	client := scriptedRewriteClient(t, "IMAP4rev1 UIDPLUS", func(w *rewriteWire) {
		tag, _ := w.command(`UID FETCH 42 \(.*\)`)
		close(fetchStarted)
		<-competingStarted
		assertLocked := func() {
			if mutex.TryLock() {
				mutex.Unlock()
				t.Error("rewrite transaction did not hold shared IMAP mutex")
			}
		}
		assertLocked()
		w.send("* 1 FETCH (%s BODY[] {%d}\r\n%s)\r\n%s OK fetched\r\n", rewriteMetadata, len(rewriteOriginal), rewriteOriginal, tag)
		w.append("OK [APPENDUID 9 99] appended", assertLocked)
		tag, _ = w.command(`UID STORE 42 \+FLAGS.SILENT \(\\Deleted\)`)
		assertLocked()
		w.send("%s OK stored\r\n", tag)
		// UID 7 is also deleted. Only UID 42 may be expunged, never EXPUNGE.
		tag, _ = w.command(`UID EXPUNGE 42`)
		assertLocked()
		w.send("* 1 EXPUNGE\r\n%s OK expunged\r\n", tag)
		tag, _ = w.command(`UID FETCH 7 \(.*BODY\.PEEK\[\].*\)`)
		body := "Subject: second\r\nContent-Type: text/plain\r\n\r\nsecond body"
		w.send("* 1 FETCH (UID 7 FLAGS (\\Deleted) BODY[] {%d}\r\n%s)\r\n%s OK fetched\r\n", len(body), body, tag)
	})
	input := make(chan classificationResult, 1)
	input <- rewriteInput()
	close(input)
	output := rewriteResults(context.Background(), client, 9, &mutex, input)
	<-fetchStarted
	competingDone := make(chan error, 1)
	go func() {
		close(competingStarted)
		mutex.Lock()
		defer mutex.Unlock()
		message, err := fetchMessage(context.Background(), client, 7, 1024)
		if err == nil && firstHeader(message.Headers, "Subject") != "second" {
			err = fmt.Errorf("competing fetch headers = %v", message.Headers)
		}
		competingDone <- err
	}()
	count := 0
	for result := range output {
		count++
		if result.err != nil || result.uid != 99 || result.subject != "[selling] hello" || result.skipped != "" {
			t.Errorf("rewrite result = %+v", result)
		}
	}
	if count != 1 {
		t.Errorf("forwarded %d results, want 1", count)
	}
	if err := <-competingDone; err != nil {
		t.Fatal(err)
	}
	rewriteBarrier(t, client)
}
