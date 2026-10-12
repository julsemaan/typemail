package typemail

import (
	"reflect"
	"testing"
)

func TestConfiguration(t *testing.T) {
	criteria := "Purchase receipts."
	disabled := false
	cfg := Config{Classes: map[string]ClassConfig{
		"selling":    {Criteria: &criteria},
		"suspicious": {Enabled: &disabled},
		"Receipts":   {Criteria: &criteria},
		"archived":   {Enabled: &disabled, Criteria: &criteria},
	}}
	classifier, err := testClassifier(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := []Class{"Receipts", "newsletter", "phishing", "regular", "selling", "spam"}
	if !reflect.DeepEqual(classifier.Classes(), want) || classifier.criteria["selling"] != criteria {
		t.Fatalf("classes = %v, criteria = %v", classifier.Classes(), classifier.criteria)
	}
	criteria = "changed"
	disabled = true
	delete(cfg.Classes, "Receipts")
	names := classifier.Classes()
	names[0] = "changed"
	other, err := testClassifier(t, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(classifier.Classes(), want) || classifier.criteria["selling"] != "Purchase receipts." || other.criteria["selling"] != classCriteria["selling"] || len(other.Classes()) != 6 {
		t.Fatal("classifiers share mutable configuration")
	}
	for _, cfg := range []Config{{}, {Classes: map[string]ClassConfig{}}, {Classes: map[string]ClassConfig{"selling": {}}}} {
		c, err := testClassifier(t, cfg)
		if err != nil || !reflect.DeepEqual(c.criteria, classCriteria) {
			t.Fatalf("default criteria = %v, error = %v", c, err)
		}
	}
}

func TestClassifierRejectsInvalidConfiguration(t *testing.T) {
	blank, criteria, disabled := " \n", "Receipts", false
	for _, name := range []string{"", " ", " receipts", "receipts ", "a\nb", "a\u007fb"} {
		if _, err := testClassifier(t, Config{Classes: map[string]ClassConfig{name: {Criteria: &criteria}}}); err == nil {
			t.Errorf("accepted invalid name %q", name)
		}
	}
	for _, config := range []ClassConfig{{}, {Enabled: &disabled}, {Criteria: &blank}, {Enabled: &disabled, Criteria: &blank}} {
		if _, err := testClassifier(t, Config{Classes: map[string]ClassConfig{"receipts": config}}); err == nil {
			t.Errorf("accepted invalid custom config %#v", config)
		}
	}
	if _, err := testClassifier(t, Config{Classes: map[string]ClassConfig{"selling": {Criteria: &blank}}}); err == nil {
		t.Fatal("accepted blank built-in criteria")
	}
	cfg := Config{Classes: map[string]ClassConfig{}}
	for name := range classCriteria {
		cfg.Classes[name] = ClassConfig{Enabled: &disabled}
	}
	if _, err := testClassifier(t, cfg); err == nil {
		t.Fatal("accepted no active classes")
	}
	cfg.Classes["receipts"] = ClassConfig{Enabled: &disabled, Criteria: &criteria}
	if _, err := testClassifier(t, cfg); err == nil {
		t.Fatal("disabled custom class must not make configuration active")
	}
}

func TestValidateConfig(t *testing.T) {
	blank := " "
	for _, cfg := range []Config{{}, {SkipSenderDomains: []string{" EXAMPLE.COM "}}, {Classes: map[string]ClassConfig{"custom": {Criteria: &blank}}}, {SkipSenderDomains: []string{"bad..com"}}} {
		validationErr := ValidateConfig(cfg)
		_, constructorErr := testClassifier(t, cfg)
		if (validationErr == nil) != (constructorErr == nil) || validationErr != nil && validationErr.Error() != constructorErr.Error() {
			t.Fatalf("validation=%v constructor=%v", validationErr, constructorErr)
		}
	}
	if _, err := New(nil, Config{}); err == nil {
		t.Fatal("accepted nil client")
	}
}
