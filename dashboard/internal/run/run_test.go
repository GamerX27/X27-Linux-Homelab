package run

import (
	"strings"
	"testing"
	"time"
)

func TestStream(t *testing.T) {
	var got []string
	out, err := Stream(10*time.Second, func(l string) { got = append(got, l) },
		"sh", "-c", `echo one; printf 'two\rthree\r\n'; echo; echo four >&2; exit 3`)
	if err == nil || !strings.Contains(err.Error(), "four") {
		t.Fatalf("error %v", err)
	}
	if strings.Join(got, "|") != "one|two|three|four" || out != "one\ntwo\nthree\nfour" {
		t.Fatalf("lines %q out %q", got, out)
	}
	if out, err := Stream(10*time.Second, nil, "sh", "-c", "echo ok"); err != nil || out != "ok" {
		t.Fatalf("ok: %q %v", out, err)
	}
	if _, err := Stream(100*time.Millisecond, nil, "sleep", "5"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout: %v", err)
	}
}
