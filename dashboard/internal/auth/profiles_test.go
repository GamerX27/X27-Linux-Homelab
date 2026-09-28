package auth

import "testing"

func TestGravatar(t *testing.T) {
	// Gravatar's documented example: SHA-256 of the trimmed, lower-cased address.
	if h := GravatarHash("  MyEmailAddress@example.com "); h != "84059b07d4be67b806386c0aad8070a23f18836bbaae342275dc0a83414c32ee" {
		t.Fatal(h)
	}
	p := NewProfiles(t.TempDir())
	if _, err := p.SetEmail("alice", "not an email"); err == nil {
		t.Fatal("bad email accepted")
	}
	h, err := p.SetEmail("alice", "Alice@Example.com")
	if err != nil || h != GravatarHash("alice@example.com") || p.Gravatar("alice") != h {
		t.Fatal(h, err)
	}
	if _, err := p.SetEmail("alice", ""); err != nil || p.Gravatar("alice") != "" {
		t.Fatal("clear failed")
	}
}
