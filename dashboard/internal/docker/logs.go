package docker

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A LogEntry is one line a container wrote.
type LogEntry struct {
	Time      int64  `json:"time"`   // unix milliseconds
	Stream    string `json:"stream"` // stdout or stderr
	Container string `json:"container,omitempty"`
	Message   string `json:"message"`

	ts time.Time // Docker's timestamp, to the nanosecond, for following
}

type LogQuery struct {
	Tail   int       // lines from the end of each container's log; 0 for all of them (since/until bound it)
	Since  time.Time // only after this (exclusive, so a cursor doesn't repeat its last line)
	Until  time.Time
	Stream string // "", "stdout" or "stderr"
}

type LogResult struct {
	Entries []LogEntry `json:"entries"`
	Cursor  string     `json:"cursor,omitempty"` // pass back as since to get what came after
}

// MaxLogLines caps what one request returns, after merging containers.
const MaxLogLines = 5000

// ParseCursor reads a cursor (or since) value: an RFC 3339 time or unix seconds.
func ParseCursor(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		sec := int64(f)
		return time.Unix(sec, int64((f-float64(sec))*1e9)), nil
	}
	return time.Time{}, errors.New("bad since or cursor")
}

// LogEntries reads the log of one container as lines.
func (c *Client) LogEntries(id string, q LogQuery) ([]LogEntry, error) {
	var info struct {
		Name   string             `json:"Name"`
		Config struct{ Tty bool } `json:"Config"`
	}
	if err := c.do("GET", "/containers/"+id+"/json", nil, &info); err != nil {
		return nil, err
	}
	v := url.Values{"stdout": {"1"}, "stderr": {"1"}, "timestamps": {"1"}}
	switch q.Stream {
	case "stdout":
		v.Set("stderr", "0")
	case "stderr":
		v.Set("stdout", "0")
	}
	if q.Tail > 0 {
		v.Set("tail", strconv.Itoa(q.Tail))
	}
	if !q.Since.IsZero() {
		v.Set("since", fmt.Sprintf("%d.%09d", q.Since.Unix(), q.Since.Nanosecond()))
	}
	if !q.Until.IsZero() {
		v.Set("until", fmt.Sprintf("%d.%09d", q.Until.Unix(), q.Until.Nanosecond()))
	}
	req, _ := http.NewRequest("GET", "http://docker/containers/"+id+"/logs?"+v.Encode(), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("docker: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	name := strings.TrimPrefix(info.Name, "/")
	var out []LogEntry
	add := func(stream string, chunk []byte) {
		for _, line := range bytes.Split(bytes.TrimRight(chunk, "\n"), []byte("\n")) {
			e := parseLogLine(string(line), stream)
			if !q.Since.IsZero() && !e.ts.IsZero() && !e.ts.After(q.Since) {
				continue
			}
			e.Container = name
			out = append(out, e)
		}
	}
	if info.Config.Tty {
		if len(body) > 0 {
			add("stdout", body)
		}
		return out, nil
	}
	for _, f := range demuxFrames(body) {
		add(f.stream, f.data)
	}
	return out, nil
}

type frame struct {
	stream string
	data   []byte
}

// demuxFrames splits a non-TTY log stream: each frame has an 8-byte header, the stream
// (1 stdout, 2 stderr) first and the length last.
func demuxFrames(b []byte) []frame {
	var out []frame
	for len(b) >= 8 {
		stream := "stdout"
		if b[0] == 2 {
			stream = "stderr"
		}
		n := int(binary.BigEndian.Uint32(b[4:8]))
		b = b[8:]
		if n > len(b) {
			n = len(b)
		}
		// A long line can come in several frames; join it to the previous one of the stream.
		if l := len(out); l > 0 && out[l-1].stream == stream && len(out[l-1].data) > 0 && out[l-1].data[len(out[l-1].data)-1] != '\n' {
			out[l-1].data = append(out[l-1].data, b[:n]...)
		} else {
			out = append(out, frame{stream, append([]byte(nil), b[:n]...)})
		}
		b = b[n:]
	}
	return out
}

func parseLogLine(line, stream string) LogEntry {
	e := LogEntry{Stream: stream, Message: line}
	if ts, rest, ok := strings.Cut(line, " "); ok {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			e.ts, e.Time, e.Message = t, t.UnixMilli(), rest
		}
	}
	e.Message = strings.TrimRight(e.Message, "\r")
	return e
}

// MergedLogs reads several containers' logs and merges them by time. The cursor is the
// newest line's time, so following picks up after it in every container.
func (c *Client) MergedLogs(ids []string, q LogQuery) (LogResult, error) {
	type res struct {
		es  []LogEntry
		err error
	}
	results := make([]res, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			es, err := c.LogEntries(id, q)
			results[i] = res{es, err}
		}()
	}
	wg.Wait()
	var all []LogEntry
	var firstErr error
	for _, r := range results {
		if r.err != nil && firstErr == nil {
			firstErr = r.err
		}
		all = append(all, r.es...)
	}
	if len(all) == 0 && firstErr != nil {
		return LogResult{Entries: []LogEntry{}}, firstErr
	}
	sort.SliceStable(all, func(a, b int) bool { return all[a].ts.Before(all[b].ts) })
	if len(all) > MaxLogLines {
		all = all[len(all)-MaxLogLines:]
	}
	out := LogResult{Entries: all}
	if out.Entries == nil {
		out.Entries = []LogEntry{}
	}
	cursor := q.Since
	for _, e := range all {
		if e.ts.After(cursor) {
			cursor = e.ts
		}
	}
	if !cursor.IsZero() {
		out.Cursor = cursor.UTC().Format(time.RFC3339Nano)
	}
	return out, nil
}

// StackContainers is the ids of a compose project's containers.
func StackContainers(cs []Container, project string) []string {
	var ids []string
	for _, c := range cs {
		if c.Labels[labelProject] == project {
			ids = append(ids, c.ID)
		}
	}
	return ids
}
