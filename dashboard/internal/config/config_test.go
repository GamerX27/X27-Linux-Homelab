package config

import (
	"path/filepath"
	"regexp"
	"testing"
)

func TestHashCheck(t *testing.T) {
	h := Hash("secret")
	if !Check(h, "secret") || Check(h, "Secret") || Check("", "") || Check("zz:zz", "secret") {
		t.Fatal("hash check wrong")
	}
	if Hash("secret") == h {
		t.Fatal("hash not salted")
	}
}

func TestPairPassword(t *testing.T) {
	pw := NewPairPassword()
	if !regexp.MustCompile(`^([2-9A-Z]{4}-){4}[2-9A-Z]{4}$`).MatchString(pw) {
		t.Fatalf("bad password %q", pw)
	}
	if got := NormalizePairPassword(" " + lower(pw[:9]) + pw[10:] + " "); got != pw {
		t.Fatalf("normalize: %q != %q", got, pw)
	}
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func TestSaveLoad(t *testing.T) {
	Path = filepath.Join(t.TempDir(), "dashboard.conf")
	c, err := Load()
	if err != nil || c != Default() {
		t.Fatalf("missing file: %+v %v", c, err)
	}
	c = Config{Mode: ModeNode, Port: 9443, PairHash: "a:b", TokenHash: "c:d", Presets: "https://example.org/a/b"}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil || got != c {
		t.Fatalf("round trip: %+v %v", got, err)
	}
}
