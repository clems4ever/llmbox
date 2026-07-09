package guest

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestStripANSI checks ANSI and carriage-return removal.
func TestStripANSI(t *testing.T) {
	in := "\x1b[31mhello\x1b[0m\rworld\x1b]0;title\x07!"
	if got := string(stripANSI([]byte(in))); got != "helloworld!" {
		t.Fatalf("stripANSI = %q, want %q", got, "helloworld!")
	}
}

// TestTranscriptWaitForFindsMatch returns the first matching regexp and advances
// past it, so a later scan with an overlapping pattern continues after the match.
func TestTranscriptWaitForFindsMatch(t *testing.T) {
	tr := newTranscript()
	go func() {
		tr.append([]byte("noise\n"))
		tr.append([]byte("visit https://claude.com/cai/oauth/authorize?a=1&code_challenge=x&state=abc now\n"))
		tr.append([]byte("ready https://claude.ai/s/session-1\n"))
	}()

	res := []*regexp.Regexp{authorizeURLRe, sessionURLRe}
	match, idx, _, err := tr.waitForAny(res, 2*time.Second)
	if err != nil || idx != 0 {
		t.Fatalf("first match: idx=%d err=%v", idx, err)
	}
	if !strings.Contains(match, "oauth/authorize") {
		t.Fatalf("authorize match = %q", match)
	}

	// The next scan for the (broad) session regex must skip the already-consumed
	// authorize URL and land on the real session URL.
	match2, _, _, err := tr.waitForAny([]*regexp.Regexp{sessionURLRe}, 2*time.Second)
	if err != nil {
		t.Fatalf("second match: %v", err)
	}
	if match2 != "https://claude.ai/s/session-1" {
		t.Fatalf("session match = %q, want the session URL (not the authorize URL)", match2)
	}
}

// TestTranscriptWaitForStreamEnds errors with a tail when the stream ends first.
func TestTranscriptWaitForStreamEnds(t *testing.T) {
	tr := newTranscript()
	tr.append([]byte("some output before the end"))
	go tr.close(nil)
	_, _, tail, err := tr.waitForAny([]*regexp.Regexp{sessionURLRe}, 2*time.Second)
	if err == nil {
		t.Fatal("want error when stream ends before a match")
	}
	if !strings.Contains(tail, "some output") {
		t.Fatalf("tail = %q, want it to include the box output", tail)
	}
}

// TestTranscriptWaitForTimesOut errors with a tail when no match arrives in time.
func TestTranscriptWaitForTimesOut(t *testing.T) {
	tr := newTranscript()
	tr.append([]byte("still working"))
	_, _, tail, err := tr.waitForAny([]*regexp.Regexp{sessionURLRe}, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want timeout", err)
	}
	if !strings.Contains(tail, "still working") {
		t.Fatalf("tail = %q", tail)
	}
}

// TestTranscriptLogsTail returns only the trailing lines.
func TestTranscriptLogsTail(t *testing.T) {
	tr := newTranscript()
	tr.append([]byte("line1\nline2\nline3\nline4\n"))
	if got := tr.logs(2); got != "line3\nline4" {
		t.Fatalf("logs(2) = %q, want last two lines", got)
	}
}

// TestExecCapsOutput truncates output past the cap and marks it.
func TestExecCapsOutput(t *testing.T) {
	big := strings.Repeat("a", maxExecOutput+100)
	got := capOutput([]byte(big))
	if !strings.HasSuffix(got, "[output truncated]") {
		t.Fatalf("capped output missing marker: %q...", got[:40])
	}
	if len(got) >= len(big) {
		t.Fatalf("output not truncated: len=%d", len(got))
	}
}
