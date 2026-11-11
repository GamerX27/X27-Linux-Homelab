package docker

import (
	"encoding/binary"
	"testing"
	"time"
)

func mkframe(stream byte, s string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
	return append(h, s...)
}

func TestDemuxFramesAndParse(t *testing.T) {
	var b []byte
	b = append(b, mkframe(1, "2026-11-11T10:00:00.000000001Z hello\n")...)
	b = append(b, mkframe(2, "2026-11-11T10:00:01.5Z oops, a long ")...)
	b = append(b, mkframe(2, "line\n")...)
	b = append(b, mkframe(1, "no timestamp\r\n")...)
	fs := demuxFrames(b)
	if len(fs) != 3 || fs[1].stream != "stderr" || string(fs[1].data) != "2026-11-11T10:00:01.5Z oops, a long line\n" {
		t.Fatalf("%q", fs)
	}
	e := parseLogLine("2026-11-11T10:00:01.5Z oops", "stderr")
	if e.Message != "oops" || e.Time != time.Date(2026, 11, 11, 10, 0, 1, 5e8, time.UTC).UnixMilli() {
		t.Fatalf("%+v", e)
	}
	if e := parseLogLine("plain\r", "stdout"); e.Message != "plain" || e.Time != 0 {
		t.Fatalf("%+v", e)
	}
}

func TestParseCursor(t *testing.T) {
	want := time.Date(2026, 11, 11, 10, 0, 0, 123456789, time.UTC)
	if got, err := ParseCursor(want.Format(time.RFC3339Nano)); err != nil || !got.Equal(want) {
		t.Fatal(got, err)
	}
	if _, err := ParseCursor("yesterday"); err == nil {
		t.Fatal("accepted junk")
	}
}
