package typemail

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// ClassConfig overrides a class. Nil fields preserve defaults.
// Custom classes require nonblank criteria even when disabled.
type ClassConfig struct {
	Enabled  *bool   `toml:"enabled"`
	Criteria *string `toml:"criteria"`
}

// Config configures sender bypasses and classes by exact name.
// The zero value preserves all built-in classes and criteria.
type Config struct {
	Classes           map[string]ClassConfig `toml:"classes"`
	SkipSenderDomains []string               `toml:"skip_sender_domains"`
}

var classCriteria = map[string]string{
	string(ClassRegular):    "Expected personal, business, or transactional correspondence.",
	string(ClassSelling):    "Email that sollicits or promotes a purchase of a consumer product",
	string(ClassNewsletter): "Expected recurring editorial or informational mail, such as news digests, articles, or community updates. Exclude purchase-focused promotions, unsolicited junk, and deceptive mail.",
	string(ClassSpam):       "Unwanted bulk mail, generic solicitation, or low-quality junk.",
	string(ClassPhishing):   "Deceptive requests for credentials, money, sensitive data, or harmful actions.",
	string(ClassSuspicious): "Concerning or ambiguous mail that does not clearly fit another class.",
}

// ValidateConfig validates email policy without requiring credentials.
func ValidateConfig(cfg Config) error {
	_, _, err := resolveConfig(cfg)
	return err
}

func resolveConfig(cfg Config) (map[string]string, []string, error) {
	criteria := make(map[string]string, len(classCriteria)+len(cfg.Classes))
	for name, value := range classCriteria {
		criteria[name] = value
	}
	for name, config := range cfg.Classes {
		if err := validateClassName(name); err != nil {
			return nil, nil, err
		}
		_, builtin := classCriteria[name]
		if config.Criteria == nil && !builtin {
			return nil, nil, fmt.Errorf("class %q requires criteria", name)
		}
		if config.Criteria != nil {
			if strings.TrimSpace(*config.Criteria) == "" {
				return nil, nil, fmt.Errorf("class %q criteria must not be blank", name)
			}
			criteria[name] = *config.Criteria
		}
		if config.Enabled != nil && !*config.Enabled {
			delete(criteria, name)
		}
	}
	if len(criteria) == 0 {
		return nil, nil, errors.New("at least one active class is required")
	}

	domains := make([]string, len(cfg.SkipSenderDomains))
	for i, domain := range cfg.SkipSenderDomains {
		domain = strings.ToLower(strings.TrimSpace(domain))
		if !validDomain(domain) {
			return nil, nil, fmt.Errorf("invalid skip sender domain %q", cfg.SkipSenderDomains[i])
		}
		domains[i] = domain
	}

	return criteria, domains, nil
}

func validateClassName(name string) error {
	if name == "" || strings.TrimSpace(name) != name || strings.ContainsFunc(name, unicode.IsControl) {
		return fmt.Errorf("invalid class name %q", name)
	}
	return nil
}

func validDomain(domain string) bool {
	if domain == "" || len(domain) > 253 {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
				return false
			}
		}
	}
	return true
}
