// Package run executes host commands with an argument vector (never through a shell)
// and a timeout, returning trimmed combined output.
package run

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Cmd runs name with args. On failure the error carries the command's own output,
// which is what the UI shows.
func Cmd(timeout time.Duration, name string, args ...string) (string, error) {
	return CmdInput(timeout, "", name, args...)
}

// CmdInput is Cmd with stdin, for secrets that shouldn't show up in the process list.
func CmdInput(timeout time.Duration, stdin, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if ctx.Err() == context.DeadlineExceeded {
		return s, fmt.Errorf("%s timed out after %s", name, timeout)
	}
	if err != nil {
		if s == "" {
			return s, fmt.Errorf("%s: %w", name, err)
		}
		return s, errors.New(s)
	}
	return s, nil
}

// ExitCode is Cmd for commands whose exit status means something (rpm-ostree's 77).
// It returns -1 when the command couldn't run at all.
func ExitCode(timeout time.Duration, name string, args ...string) (string, int) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return strings.TrimSpace(string(out)), 0
	case errors.As(err, &ee):
		return strings.TrimSpace(string(out)), ee.ExitCode()
	default:
		return strings.TrimSpace(string(out)) + err.Error(), -1
	}
}
