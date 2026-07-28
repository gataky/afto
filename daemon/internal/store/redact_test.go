package store

import "testing"

func TestDefaultRedactionPatterns(t *testing.T) {
	r, err := NewRedactor(nil)
	if err != nil {
		t.Fatal(err)
	}
	skip := []string{
		"aws configure set aws_access_key_id AKIAIOSFODNN7EXAMPLE",
		"echo $AWS_SECRET_ACCESS_KEY",
		"mysql -u root --password=hunter2",
		"curl --token abc123 https://api",
		"deploy --secret s3cr3t",
		"http --api-key=xyz service/endpoint",
		`curl -H "Authorization: Bearer eyJhbGciOi" https://api`,
		`curl -H "authorization: basic dXNlcjpwYXNz" https://api`,
		"export GITHUB_TOKEN=ghx_abc",
		"export MY_SECRET=shh",
		"  export DEPLOY_KEY=abc",
		"export DB_PASSWORD=pw",
		"slack-cli --auth xoxb-123456-abcdef",
		"git push https://ghp_0123456789abcdefABCDEF0123456789abcd@github.com/x/y",
		"gh auth login --with-token github_pat_abc",
		" ls -la", // leading space: user opted out of history
	}
	for _, cmd := range skip {
		if !r.Skip(cmd) {
			t.Errorf("must skip: %q", cmd)
		}
	}

	keep := []string{
		"git checkout main",
		"ls -la",
		"echo password reset email sent", // the word alone isn't a credential flag
		"vim secrets.md",
		"export PATH=$PATH:/opt/bin",
		"make token-ring-demo",
	}
	for _, cmd := range keep {
		if r.Skip(cmd) {
			t.Errorf("must keep: %q", cmd)
		}
	}
}

func TestExtraPatternsExtendDefaults(t *testing.T) {
	r, err := NewRedactor([]string{`(?i)corp-vault`})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Skip("corp-vault read secret/db") {
		t.Error("extra pattern must apply")
	}
	if !r.Skip(" ls") {
		t.Error("defaults must still apply")
	}
}

func TestBadExtraPatternFailsLoudly(t *testing.T) {
	if _, err := NewRedactor([]string{`([`}); err == nil {
		t.Fatal("invalid pattern must be an error, not silently dropped")
	}
}
