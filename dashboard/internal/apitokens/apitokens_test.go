package apitokens

import (
	"strings"
	"testing"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/tools"
)

func TestTokens(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	tok, secret, err := s.Create("sindre", "Claude Code", tools.Operate, 0)
	if err != nil || !strings.HasPrefix(secret, Prefix) {
		t.Fatal(tok, secret, err)
	}
	if _, _, err := s.Create("sindre", " ", tools.Read, 0); err == nil {
		t.Error("empty name accepted")
	}
	got, err := s.Verify(secret)
	if err != nil || got.Owner != "sindre" || got.Scope != tools.Operate || got.Hash != "" {
		t.Fatal(got, err)
	}
	if _, err := s.Verify(secret + "x"); err == nil {
		t.Error("wrong secret accepted")
	}
	if _, err := s.Verify("Bearer nonsense"); err == nil {
		t.Error("garbage accepted")
	}
	// Survives a restart, with no secrets in the list.
	s2, _ := Open(dir)
	if l := s2.List(); len(l) != 1 || l[0].Hash != "" || l[0].LastUsed == 0 {
		t.Fatal(l)
	}
	if _, err := s2.Verify(secret); err != nil {
		t.Fatal(err)
	}
	if err := s2.Delete(tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Verify(secret); err == nil {
		t.Error("deleted token still works")
	}
}
