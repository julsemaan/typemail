package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestParseNetworks(t *testing.T) {
	networks, err := parseNetworks("127.0.0.0/8, 192.0.2.0/24 [::1]/128 [2001:db8::]/32 198.51.100.1")
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{"127.0.0.1", "192.0.2.255", "::1", "2001:db8::99", "198.51.100.1"} {
		found := false
		for _, n := range networks {
			found = found || n.Contains(net.ParseIP(addr))
		}
		if !found {
			t.Errorf("trusted address %s not found", addr)
		}
	}
	for _, value := range []string{"", "cidr:/etc/postfix/trusted", "127.0.0.1/99", "$other", "bad"} {
		if _, err := parseNetworks(value); err == nil {
			t.Errorf("accepted invalid networks %q", value)
		}
	}
}

func TestLoadClassifier(t *testing.T) {
	env := func(string) string { return "key" }
	if _, err := loadClassifier(nil, env); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-debug"}, {"-concurrency", "10"}, {"extra"}} {
		if _, err := loadClassifier(args, env); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	if _, err := loadClassifier(nil, func(string) string { return "" }); err == nil {
		t.Fatal("accepted missing API key")
	}
	path := filepath.Join(t.TempDir(), "typemail.toml")
	if err := os.WriteFile(path, []byte("skip_sender_domains = ['semaan.ca']\n[classes.receipts]\ncriteria = 'Invoices'"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := loadClassifier([]string{"-config", path}, env)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, class := range c.Classes() {
		found = found || class == "receipts"
	}
	if !found {
		t.Fatal("custom class absent")
	}
	if err := os.WriteFile(path, []byte("[classes.spam]\nEnabled = false"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadClassifier([]string{"-config", path}, env); err == nil {
		t.Fatal("accepted unknown setting case")
	}
}
