package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadClassifierConfig(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		bad           bool
	}{
		{"defaults", "", false},
		{"sender domains", "skip_sender_domains = ['semaan.ca']", false},
		{"empty sender domains", "skip_sender_domains = []", false},
		{"sender domain casing", "Skip_Sender_Domains = ['semaan.ca']", true},
		{"empty domain", "skip_sender_domains = ['']", true},
		{"mailbox domain", "skip_sender_domains = ['a@semaan.ca']", true},
		{"URL domain", "skip_sender_domains = ['https://semaan.ca']", true},
		{"wildcard domain", "skip_sender_domains = ['*.semaan.ca']", true},
		{"custom", "[classes.receipts]\ncriteria = 'Invoices'", false},
		{"unknown", "[classes.spam]\nextra = true", true},
		{"case", "[classes.spam]\nEnabled = false", true},
		{"invalid criteria", "[classes.receipts]\ncriteria = ''", true},
		{"missing custom criteria", "[classes.receipts]\nenabled = false", true},
		{"invalid name", "[classes.' bad ']\ncriteria = 'Bad'", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "typemail.toml")
			if err := os.WriteFile(path, []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadClassifierConfig(path)
			if tc.name == "sender domains" && (len(cfg.SkipSenderDomains) != 1 || cfg.SkipSenderDomains[0] != "semaan.ca") {
				t.Fatalf("sender domains = %v", cfg.SkipSenderDomains)
			}
			if (err != nil) != tc.bad {
				t.Fatalf("error = %v, want bad=%v", err, tc.bad)
			}
		})
	}
	if _, err := LoadClassifierConfig(""); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadClassifierConfig(filepath.Join(t.TempDir(), "absent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
}
