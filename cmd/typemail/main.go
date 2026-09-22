package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"unicode"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"typemail"
)

const (
	defaultConcurrency = 5
	maxTextBytes       = 100 << 10
)

var errMessageFailures = errors.New("one or more messages failed")

// config contains validated command-line and environment configuration.
type config struct {
	imapAddr      string
	imapUsername  string
	imapPassword  string
	openRouterKey string
	concurrency   int
}

func loadConfig(args []string, getenv func(string) string) (config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}

	flags := flag.NewFlagSet("typemail", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	concurrency := flags.Int("concurrency", defaultConcurrency, "maximum concurrent classifications")
	if err := flags.Parse(args); err != nil {
		return config{}, fmt.Errorf("parse flags: %w", err)
	}
	if *concurrency < 1 {
		return config{}, errors.New("concurrency must be at least 1")
	}

	values := make(map[string]string, 4)
	for _, name := range []string{
		"TYPEMAIL_IMAP_ADDR",
		"TYPEMAIL_IMAP_USERNAME",
		"TYPEMAIL_IMAP_PASSWORD",
		"OPENROUTER_API_KEY",
	} {
		value := getenv(name)
		if strings.TrimSpace(value) == "" {
			return config{}, fmt.Errorf("%s is required", name)
		}
		values[name] = value
	}

	return config{
		imapAddr:      values["TYPEMAIL_IMAP_ADDR"],
		imapUsername:  values["TYPEMAIL_IMAP_USERNAME"],
		imapPassword:  values["TYPEMAIL_IMAP_PASSWORD"],
		openRouterKey: values["OPENROUTER_API_KEY"],
		concurrency:   *concurrency,
	}, nil
}

type classificationJob struct {
	uid     imap.UID
	from    string
	subject string
	message typemail.Message
	err     error
}

type classificationResult struct {
	uid     imap.UID
	from    string
	subject string
	result  typemail.Result
	err     error
}

type classifyFunc func(context.Context, typemail.Message) (typemail.Result, error)

func startWorkers(ctx context.Context, concurrency int, jobs <-chan classificationJob, classify classifyFunc) <-chan classificationResult {
	results := make(chan classificationResult)
	var workers sync.WaitGroup
	workers.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-jobs:
					if !ok {
						return
					}
					if ctx.Err() != nil {
						return
					}

					result := typemail.Result{}
					err := job.err
					if err == nil {
						result, err = classify(ctx, job.message)
					}
					out := classificationResult{
						uid:     job.uid,
						from:    job.from,
						subject: job.subject,
						result:  result,
						err:     err,
					}
					select {
					case results <- out:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	go func() {
		workers.Wait()
		close(results)
	}()
	return results
}

func fetchMessage(ctx context.Context, client *imapclient.Client, uid imap.UID) (typemail.Message, error) {
	if err := ctx.Err(); err != nil {
		return typemail.Message{}, err
	}

	section := &imap.FetchItemBodySection{Peek: true}
	command := client.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{section},
	})

	message := typemail.Message{}
	found := false
	var parseErr error
	response := command.Next()
	if response != nil {
		for {
			item := response.Next()
			if item == nil {
				break
			}
			body, ok := item.(imapclient.FetchItemDataBodySection)
			if !ok {
				continue
			}
			found = true
			if body.Literal == nil {
				parseErr = errors.New("IMAP returned an empty message body")
				continue
			}
			message, parseErr = typemail.ParseMessage(body.Literal, maxTextBytes)
		}
	}
	closeErr := command.Close()
	if parseErr != nil {
		return typemail.Message{}, fmt.Errorf("parse UID %d: %w", uid, parseErr)
	}
	if closeErr != nil {
		return typemail.Message{}, fmt.Errorf("fetch UID %d: %w", uid, closeErr)
	}
	if response == nil || !found {
		return typemail.Message{}, fmt.Errorf("fetch UID %d: message body not returned", uid)
	}
	return message, nil
}

func firstHeader(headers map[string][]string, key string) string {
	values := headers[key]
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func sanitizeField(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
}

type runSummary struct {
	counts   map[typemail.Class]int
	failures int
	total    int
}

func outputResults(stdout, stderr io.Writer, results <-chan classificationResult) runSummary {
	writer := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "UID\tFROM\tSUBJECT\tCLASS\tCONFIDENCE")
	summary := runSummary{counts: make(map[typemail.Class]int)}
	for result := range results {
		summary.total++
		if result.err != nil {
			summary.failures++
			_, _ = fmt.Fprintf(stderr, "UID %d: %v\n", result.uid, result.err)
			continue
		}
		summary.counts[result.result.Class]++
		_, _ = fmt.Fprintf(writer, "%d\t%s\t%s\t%s\t%.2f\n",
			result.uid,
			sanitizeField(result.from),
			sanitizeField(result.subject),
			result.result.Class,
			result.result.Confidence,
		)
	}
	_ = writer.Flush()
	return summary
}

func writeSummary(w io.Writer, summary runSummary) {
	_, _ = fmt.Fprintln(w, "\nTotals:")
	_, _ = fmt.Fprintf(w, "Messages: %d\n", summary.total)
	for _, class := range []typemail.Class{
		typemail.ClassLegitimate,
		typemail.ClassSpam,
		typemail.ClassPhishing,
		typemail.ClassMarketing,
		typemail.ClassSuspicious,
	} {
		_, _ = fmt.Fprintf(w, "%s: %d\n", class, summary.counts[class])
	}
	_, _ = fmt.Fprintf(w, "Failures: %d\n", summary.failures)
}

func run(ctx context.Context, cfg config, stdout, stderr io.Writer) error {
	classifier, err := typemail.NewClassifier(cfg.openRouterKey)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	client, err := imapclient.DialTLS(cfg.imapAddr, nil)
	if err != nil {
		return fmt.Errorf("connect to IMAP: %w", err)
	}
	defer func() { _ = client.Close() }()
	closeDone := make(chan struct{})
	defer close(closeDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = client.Close()
		case <-closeDone:
		}
	}()

	if err := client.Login(cfg.imapUsername, cfg.imapPassword).Wait(); err != nil {
		return fmt.Errorf("authenticate with IMAP: %w", err)
	}
	if _, err := client.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		return fmt.Errorf("select INBOX: %w", err)
	}
	search, err := client.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
	if err != nil {
		return fmt.Errorf("search INBOX: %w", err)
	}

	jobs := make(chan classificationJob)
	results := startWorkers(ctx, cfg.concurrency, jobs, classifier.Classify)
	go func() {
		defer close(jobs)
		for _, uid := range search.AllUIDs() {
			if ctx.Err() != nil {
				return
			}
			message, err := fetchMessage(ctx, client, uid)
			job := classificationJob{uid: uid, err: err}
			if err == nil {
				job.message = message
				job.from = firstHeader(message.Headers, "From")
				job.subject = firstHeader(message.Headers, "Subject")
			}
			select {
			case jobs <- job:
			case <-ctx.Done():
				return
			}
		}
	}()

	summary := outputResults(stdout, stderr, results)
	writeSummary(stdout, summary)
	if err := ctx.Err(); err != nil {
		return err
	}
	if summary.failures > 0 {
		return errMessageFailures
	}
	return nil
}

func main() {
	cfg, err := loadConfig(os.Args[1:], os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfg, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
