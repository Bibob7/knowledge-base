package secrets

import "testing"

func TestCheck(t *testing.T) {
	bad := []string{
		"-----BEGIN RSA PRIVATE KEY-----",
		"key AKIAABCDEFGHIJKLMNOP",
		"export OPENAI=sk-abcdefghijklmnopqrstuvwx",
		"password = hunter2hunter2",
		"token: abcdefgh12345",
	}
	for _, s := range bad {
		if Check(s) == nil {
			t.Errorf("should be rejected: %q", s)
		}
	}
	good := []string{
		"We use pgx instead of database/sql",
		"The password is set via an environment variable",
		"The token endpoint lives at /oauth/token",
	}
	for _, s := range good {
		if err := Check(s); err != nil {
			t.Errorf("wrongly rejected %q: %v", s, err)
		}
	}
}
