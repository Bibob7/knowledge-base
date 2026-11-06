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
			t.Errorf("sollte abgelehnt werden: %q", s)
		}
	}
	good := []string{
		"Wir nutzen pgx statt database/sql",
		"Das Passwort wird per Env-Variable gesetzt",
		"Der Token-Endpunkt liegt unter /oauth/token",
	}
	for _, s := range good {
		if err := Check(s); err != nil {
			t.Errorf("faelschlich abgelehnt %q: %v", s, err)
		}
	}
}
