package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"typemail"
)

// prepareMailbox snapshots eligible UIDs before any replacements are uploaded.
func prepareMailbox(client *imapclient.Client, rewrite bool) ([]imap.UID, uint32, error) {
	if rewrite && !client.Caps().Has(imap.CapUIDPlus) && !client.Caps().Has(imap.CapIMAP4rev2) {
		return nil, 0, errors.New("subject rewriting requires UIDPLUS or IMAP4rev2")
	}
	selected, err := client.Select("INBOX", &imap.SelectOptions{ReadOnly: !rewrite}).Wait()
	if err != nil {
		return nil, 0, fmt.Errorf("select INBOX: %w", err)
	}
	if rewrite && selected.UIDValidity == 0 {
		return nil, 0, errors.New("subject rewriting requires mailbox UIDVALIDITY")
	}
	search, err := client.UIDSearch(&imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagDeleted}}, nil).Wait()
	if err != nil {
		return nil, 0, fmt.Errorf("search INBOX: %w", err)
	}
	return search.AllUIDs(), selected.UIDValidity, nil
}

// rewriteMessage is called with the scan's IMAP mutex held throughout.
func rewriteMessage(ctx context.Context, client *imapclient.Client, uidValidity uint32, result classificationResult) classificationResult {
	if result.err != nil {
		return result
	}
	fail := func(err error) classificationResult { result.err = err; return result }
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	file, err := os.CreateTemp("", "typemail-*.eml")
	if err != nil {
		return fail(fmt.Errorf("create rewrite file: %w", err))
	}
	defer os.Remove(file.Name())
	defer file.Close()
	section := &imap.FetchItemBodySection{Peek: true}
	command := client.Fetch(imap.UIDSetNum(result.uid), &imap.FetchOptions{
		UID: true, Flags: true, InternalDate: true, BodySection: []*imap.FetchItemBodySection{section},
	})
	var uid imap.UID
	var flags []imap.Flag
	var date time.Time
	var haveFlags, haveDate, haveBody, changed bool
	var subject string
	var rewriteErr error
	response := command.Next()
	if response != nil {
		for item := response.Next(); item != nil; item = response.Next() {
			switch item := item.(type) {
			case imapclient.FetchItemDataUID:
				uid = item.UID
			case imapclient.FetchItemDataFlags:
				flags, haveFlags = item.Flags, true
			case imapclient.FetchItemDataInternalDate:
				date, haveDate = item.Time, true
			case imapclient.FetchItemDataBodySection:
				haveBody = item.Literal != nil
				if haveBody {
					subject, changed, rewriteErr = typemail.RewriteSubject(file, item.Literal, result.result.Class)
				}
			}
		}
	}
	// Close drains literals and all remaining responses, even after parse errors.
	closeErr := command.Close()
	if closeErr != nil {
		return fail(fmt.Errorf("fetch original: %w", closeErr))
	}
	if response == nil {
		result.skipped = "message disappeared"
		return result
	}
	if uid != result.uid {
		return fail(errors.New("fetch original: UID mismatch"))
	}
	if !haveFlags || !haveDate || date.IsZero() || !haveBody {
		return fail(errors.New("fetch original: incomplete metadata"))
	}
	for _, flag := range flags {
		if strings.EqualFold(string(flag), string(imap.FlagDeleted)) {
			result.skipped = "message is deleted"
			return result
		}
	}
	if rewriteErr != nil {
		return fail(fmt.Errorf("rewrite subject: %w", rewriteErr))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	result.subject = subject
	if !changed {
		return result
	}
	stat, err := file.Stat()
	if err != nil {
		return fail(err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	persistent := make([]imap.Flag, 0, len(flags))
	for _, flag := range flags {
		if !strings.EqualFold(string(flag), "\\Recent") && !strings.EqualFold(string(flag), string(imap.FlagDeleted)) {
			persistent = append(persistent, flag)
		}
	}
	appendCmd := client.Append("INBOX", stat.Size(), &imap.AppendOptions{Flags: persistent, Time: date})
	_, uploadErr := io.Copy(appendCmd, file)
	appendCloseErr := appendCmd.Close()
	data, appendErr := appendCmd.Wait()
	if err := errors.Join(uploadErr, appendCloseErr, appendErr); err != nil {
		return fail(fmt.Errorf("APPEND outcome may be ambiguous; original untouched, do not retry automatically: %w", err))
	}
	if data == nil || data.UID == 0 || data.UIDValidity == 0 || data.UIDValidity != uidValidity || data.UID == result.uid {
		return fail(errors.New("APPEND returned no valid replacement UID; original untouched, do not retry automatically"))
	}
	replacement := data.UID
	deletionFailure := func(err error) classificationResult {
		return fail(fmt.Errorf("replacement UID %d retained; original UID %d cleanup failed: %w", replacement, result.uid, err))
	}
	if err := ctx.Err(); err != nil {
		return deletionFailure(err)
	}
	if err := client.Store(imap.UIDSetNum(result.uid), &imap.StoreFlags{
		Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted},
	}, nil).Close(); err != nil {
		return deletionFailure(err)
	}
	if err := client.UIDExpunge(imap.UIDSetNum(result.uid)).Close(); err != nil {
		return deletionFailure(err)
	}
	result.uid = replacement
	return result
}

func rewriteResults(ctx context.Context, client *imapclient.Client, uidValidity uint32, mutex *sync.Mutex, input <-chan classificationResult) <-chan classificationResult {
	output := make(chan classificationResult)
	go func() {
		defer close(output)
		for result := range input {
			mutex.Lock()
			result = rewriteMessage(ctx, client, uidValidity, result)
			mutex.Unlock()
			output <- result
		}
	}()
	return output
}
