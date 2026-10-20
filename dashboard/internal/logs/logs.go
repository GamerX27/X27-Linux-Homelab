// Package logs reads the systemd journal for the Logs tab: the whole journal, the kernel, or
// one unit, filtered by priority, time and text, and followed with journal cursors.
package logs

import (
	"bufio"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/run"
)

const MaxLimit = 2000

type Query struct {
	Unit     string // a systemd unit, or "" for everything
	Kernel   bool   // kernel messages only (journalctl -k)
	Priority int    // 0 (emerg) … 7 (debug); show this and more important
	Since    string // 15m, 1h, 24h, 7d or boot; "" for no limit
	Grep     string // case-insensitive text filter
	Cursor   string // only entries after this one (following)
	Limit    int
}

type Entry struct {
	Time     int64  `json:"time"` // unix milliseconds
	Unit     string `json:"unit,omitempty"`
	Ident    string `json:"ident,omitempty"`
	PID      string `json:"pid,omitempty"`
	Priority int    `json:"priority"`
	Message  string `json:"message"`
}

type Result struct {
	Entries []Entry `json:"entries"`
	Cursor  string  `json:"cursor"` // pass back to get what came after
}

var (
	unitRe   = regexp.MustCompile(`^[A-Za-z0-9@._:\\-]{1,200}$`)
	cursorRe = regexp.MustCompile(`^[A-Za-z0-9=;_-]{1,500}$`)
	sinceArg = map[string]string{"15m": "-15min", "1h": "-1h", "24h": "-24h", "7d": "-7d"}
)

// Args turns a query into journalctl arguments, validating everything that goes in.
func Args(q Query) ([]string, error) {
	args := []string{"-o", "json", "--no-pager", "-q"}
	if q.Kernel {
		args = append(args, "-k")
	} else if q.Unit != "" {
		if !unitRe.MatchString(q.Unit) {
			return nil, errors.New("bad unit name")
		}
		args = append(args, "-u", q.Unit)
	}
	if q.Priority < 0 || q.Priority > 7 {
		return nil, errors.New("priority must be 0-7")
	}
	if q.Priority < 7 {
		args = append(args, "-p", strconv.Itoa(q.Priority))
	}
	switch {
	case q.Since == "boot":
		if !q.Kernel {
			args = append(args, "-b")
		}
	case q.Since != "":
		v, ok := sinceArg[q.Since]
		if !ok {
			return nil, errors.New("since must be 15m, 1h, 24h, 7d or boot")
		}
		args = append(args, "--since", v)
	}
	if q.Cursor != "" {
		if !cursorRe.MatchString(q.Cursor) {
			return nil, errors.New("bad cursor")
		}
		args = append(args, "--after-cursor", q.Cursor)
	}
	limit := q.Limit
	if limit <= 0 || limit > MaxLimit {
		limit = 500
	}
	if q.Grep != "" {
		limit = min(limit*10, 20000) // filtered afterwards
	}
	return append(args, "-n", strconv.Itoa(limit)), nil
}

// message decodes MESSAGE, which journald writes as an array of bytes when it isn't UTF-8.
func message(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var b []byte
	var nums []int
	if json.Unmarshal(raw, &nums) == nil {
		for _, n := range nums {
			b = append(b, byte(n))
		}
		return strings.ToValidUTF8(string(b), "?")
	}
	return ""
}

// Parse reads journalctl -o json output.
func Parse(out string, grep string, limit int) Result {
	r := Result{Entries: []Entry{}}
	grep = strings.ToLower(grep)
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var m map[string]json.RawMessage
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		str := func(k string) string {
			var s string
			json.Unmarshal(m[k], &s)
			return s
		}
		r.Cursor = str("__CURSOR")
		e := Entry{Unit: str("_SYSTEMD_UNIT"), Ident: str("SYSLOG_IDENTIFIER"), PID: str("_PID"), Message: message(m["MESSAGE"]), Priority: 6}
		if us, err := strconv.ParseInt(str("__REALTIME_TIMESTAMP"), 10, 64); err == nil {
			e.Time = us / 1000
		}
		if p, err := strconv.Atoi(str("PRIORITY")); err == nil {
			e.Priority = p
		}
		if grep != "" && !strings.Contains(strings.ToLower(e.Message), grep) &&
			!strings.Contains(strings.ToLower(e.Ident), grep) {
			continue
		}
		r.Entries = append(r.Entries, e)
	}
	if limit <= 0 || limit > MaxLimit {
		limit = 500
	}
	if len(r.Entries) > limit {
		r.Entries = r.Entries[len(r.Entries)-limit:]
	}
	return r
}

func Read(q Query) (Result, error) {
	args, err := Args(q)
	if err != nil {
		return Result{}, err
	}
	out, err := run.Cmd(30*time.Second, "journalctl", args...)
	if err != nil && !strings.HasPrefix(strings.TrimSpace(out), "{") {
		return Result{}, err
	}
	r := Parse(out, q.Grep, q.Limit)
	if r.Cursor == "" {
		r.Cursor = q.Cursor
	}
	return r, nil
}

type Unit struct {
	Unit        string `json:"unit"`
	Active      string `json:"active"`
	Sub         string `json:"sub"`
	Description string `json:"description"`
}

// Units lists the loaded services for the unit picker.
func Units() ([]Unit, error) {
	out, err := run.Cmd(15*time.Second, "systemctl", "list-units", "--type=service", "--all", "--no-pager", "-o", "json")
	if err != nil {
		return nil, err
	}
	var us []Unit
	if err := json.Unmarshal([]byte(out), &us); err != nil {
		return nil, err
	}
	return us, nil
}
