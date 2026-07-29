package store

import (
	"fmt"
	"regexp"
)

// defaultPatterns are the built-in secret shapes (plans/phase-1.md §6).
// A command matching ANY pattern is skipped entirely at ingest — never
// stored masked, because masked text would resurface as a broken
// suggestion. Users extend (not replace) this list via config
// [redact].extra_patterns.
var defaultPatterns = []string{
	`AKIA[0-9A-Z]{16}`, // AWS access key id
	`(?i)aws_secret`,   // AWS secret key mentions
	`(?i)(--password|--token|--secret|--api-key)[= ]\S`,   // credential flags with a value
	`(?i)authorization:\s*(bearer|basic)`,                 // HTTP auth headers (curl -H ...)
	`(?i)^\s*export\s+\w*(TOKEN|SECRET|KEY|PASSWORD)\w*=`, // exporting secret-named vars
	`xox[baprs]-`,         // Slack tokens
	`ghp_[A-Za-z0-9]{36}`, // GitHub classic PAT
	`github_pat_`,         // GitHub fine-grained PAT
	`^ `,                  // leading space: the user asked the shell to forget it; honor that
}

// Redactor decides whether a command line may be persisted.
type Redactor struct {
	patterns []*regexp.Regexp
}

// NewRedactor compiles the default patterns plus any user extras. An
// invalid extra pattern is an error (better to fail config load loudly than
// to silently persist secrets the user thought were filtered).
func NewRedactor(extra []string) (*Redactor, error) {
	all := make([]string, 0, len(defaultPatterns)+len(extra))
	all = append(all, defaultPatterns...)
	all = append(all, extra...)
	r := &Redactor{patterns: make([]*regexp.Regexp, 0, len(all))}
	for _, p := range all {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("redact: bad pattern %q: %w", p, err)
		}
		r.patterns = append(r.patterns, re)
	}
	return r, nil
}

// Skip reports whether cmd must not be persisted.
func (r *Redactor) Skip(cmd string) bool {
	for _, re := range r.patterns {
		if re.MatchString(cmd) {
			return true
		}
	}
	return false
}
