package rpc

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// oneByteReader wraps an io.Reader so every Read call returns at most one
// byte, forcing every caller upstream (bufio.Reader's fill, io.ReadFull) to
// loop across many Read calls instead of ever seeing a whole line or body in
// one shot. This is the harshest fragmentation a well-behaved client's
// writes could ever be split into by an OS pipe.
type oneByteReader struct {
	r *bytes.Reader
}

func (o *oneByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	b, err := o.r.ReadByte()
	if err != nil {
		return 0, err
	}
	p[0] = b
	return 1, nil
}

// splitReader wraps a byte slice and returns it in chunks whose sizes are
// dictated by splits (each element is a chunk length; a final short read
// covers whatever remains), simulating a client's single logical write
// arriving as an arbitrary sequence of separate Reads.
type splitReader struct {
	data   []byte
	splits []int
	pos    int
	next   int
}

func (s *splitReader) Read(p []byte) (int, error) {
	if s.pos >= len(s.data) {
		return 0, fmt.Errorf("EOF")
	}
	n := len(p)
	if s.next < len(s.splits) {
		if want := s.splits[s.next]; want < n {
			n = want
		}
		s.next++
	}
	if s.pos+n > len(s.data) {
		n = len(s.data) - s.pos
	}
	if n == 0 {
		n = 1
	}
	copy(p, s.data[s.pos:s.pos+n])
	s.pos += n
	return n, nil
}

// wireFrame builds one Content-Length-framed message for body.
func wireFrame(body string) string {
	return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
}

// realisticSession builds three concatenated frames mirroring a real
// editor's opening exchange: initialize, a didOpen with a large file body,
// and a hover request, returning both the raw bytes and the expected
// decoded bodies in order.
func realisticSession() (raw []byte, bodies []string) {
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	bigText := strings.Repeat("package main\n// line filler for a large didOpen body\n", 4000) // ~200KB
	didOpen := fmt.Sprintf(`{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"text":%q}}`, bigText)
	hover := `{"jsonrpc":"2.0","id":2,"method":"textDocument/hover","params":{}}`

	bodies = []string{initialize, didOpen, hover}
	var buf bytes.Buffer
	for _, b := range bodies {
		buf.WriteString(wireFrame(b))
	}
	return buf.Bytes(), bodies
}

// TestReadFrameOneByteReads streams a realistic multi-frame LSP session
// through readFrame with a reader that only ever returns one byte per Read
// call. Hypothesis: a header parser or body reader that assumes a Read call
// can return a whole line/body would misparse this and, most dangerously,
// desynchronize frame boundaries so a later frame's header block is missed
// entirely (surfacing as "missing Content-Length header" on a frame that
// was never actually malformed).
func TestReadFrameOneByteReads(t *testing.T) {
	raw, want := realisticSession()
	br := bufio.NewReader(&oneByteReader{r: bytes.NewReader(raw)})
	for i, w := range want {
		got, err := readFrame(br)
		if err != nil {
			t.Fatalf("frame %d: readFrame() error = %v", i, err)
		}
		if string(got) != w {
			t.Fatalf("frame %d: readFrame() = %q, want %q", i, truncate(got), truncate([]byte(w)))
		}
	}
}

// TestReadFrameRandomSplits streams the same realistic session through
// readFrame with reads fragmented at pseudo-random byte boundaries, across
// many seeds, so the split points land inside header lines, on \r/\n
// boundaries, and inside the body at different offsets each run.
func TestReadFrameRandomSplits(t *testing.T) {
	raw, want := realisticSession()
	for seed := uint64(0); seed < 50; seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			rnd := newTestRand(seed)
			var splits []int
			for i := 0; i < len(raw); {
				n := 1 + rnd.intn(37)
				splits = append(splits, n)
				i += n
			}
			br := bufio.NewReader(&splitReader{data: raw, splits: splits})
			for i, w := range want {
				got, err := readFrame(br)
				if err != nil {
					t.Fatalf("frame %d: readFrame() error = %v", i, err)
				}
				if string(got) != w {
					t.Fatalf("frame %d: readFrame() = %q, want %q", i, truncate(got), truncate([]byte(w)))
				}
			}
		})
	}
}

// TestReadFrameLargeBody covers bodies far larger than any fixed-size
// internal buffer (a bufio.Scanner's default 64KB token limit being the
// classic trap): a 10MB body, exercised through both a plain reader and a
// fragmenting one.
func TestReadFrameLargeBody(t *testing.T) {
	body := strings.Repeat("x", 10<<20)
	raw := []byte(wireFrame(body))

	t.Run("plain", func(t *testing.T) {
		got, err := readFrame(bufio.NewReader(bytes.NewReader(raw)))
		if err != nil {
			t.Fatalf("readFrame() error = %v", err)
		}
		if string(got) != body {
			t.Fatalf("readFrame() returned %d bytes, want %d", len(got), len(body))
		}
	})

	t.Run("fragmented", func(t *testing.T) {
		rnd := newTestRand(1)
		var splits []int
		for i := 0; i < len(raw); {
			n := 4096 + rnd.intn(4096)
			splits = append(splits, n)
			i += n
		}
		got, err := readFrame(bufio.NewReader(&splitReader{data: raw, splits: splits}))
		if err != nil {
			t.Fatalf("readFrame() error = %v", err)
		}
		if string(got) != body {
			t.Fatalf("readFrame() returned %d bytes, want %d", len(got), len(body))
		}
	})
}

// TestReadFrameLongHeaderLine covers a single header line longer than the
// bufio.Reader's own internal buffer (here forced small via
// bufio.NewReaderSize), which is the shape a fixed-size line buffer would
// choke on.
func TestReadFrameLongHeaderLine(t *testing.T) {
	longValue := strings.Repeat("v", 5000)
	input := fmt.Sprintf("X-Long-Header: %s\r\nContent-Length: 2\r\n\r\n{}", longValue)
	got, err := readFrame(bufio.NewReaderSize(strings.NewReader(input), 16))
	if err != nil {
		t.Fatalf("readFrame() error = %v", err)
	}
	if string(got) != "{}" {
		t.Fatalf("readFrame() = %q, want %q", got, "{}")
	}
}

// TestReadFrameHeaderVariations covers header shapes a spec-compliant
// client may legitimately send alongside Content-Length: Content-Type
// before or after it, mixed header name casing, and an unknown extra
// header, none of which should be fatal.
func TestReadFrameHeaderVariations(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"content-type after", "Content-Length: 2\r\nContent-Type: application/vscode-jsonrpc; charset=utf-8\r\n\r\n{}"},
		{"content-type before", "Content-Type: application/vscode-jsonrpc; charset=utf-8\r\nContent-Length: 2\r\n\r\n{}"},
		{"mixed case header name", "CONTENT-LENGTH: 2\r\n\r\n{}"},
		{"unknown extra header", "X-Custom-Header: whatever\r\nContent-Length: 2\r\n\r\n{}"},
		{"header value with extra whitespace", "Content-Length:    2   \r\n\r\n{}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readFrame(bufio.NewReader(strings.NewReader(tt.input)))
			if err != nil {
				t.Fatalf("readFrame() error = %v", err)
			}
			if string(got) != "{}" {
				t.Fatalf("readFrame() = %q, want %q", got, "{}")
			}
		})
	}
}

// TestReadFrameEOFClassification covers readFrame's contract for
// distinguishing a genuinely clean end of stream (nothing at all read yet
// for this frame — an ordinary session end, e.g. the client closing its
// stdin) from a stream that closes partway through a frame (mid header
// line, mid header block, or mid body — a truncated/interrupted write,
// never a normal shutdown). Only the former must satisfy
// errors.Is(err, io.EOF); every other case must not, so Server.Serve (whose
// read loop treats errors.Is(err, io.EOF) as a clean return with nothing
// logged) cannot mistake a truncated frame for a graceful shutdown and
// silently drop it.
func TestReadFrameEOFClassification(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantCleanEOF bool
	}{
		{
			name:         "nothing read yet",
			input:        "",
			wantCleanEOF: true,
		},
		{
			name:         "partial first header line, no newline",
			input:        "Content-Le",
			wantCleanEOF: false,
		},
		{
			name:         "one full header line, stream ends before terminator",
			input:        "Content-Length: 5\r\n",
			wantCleanEOF: false,
		},
		{
			name:         "headers complete, stream ends before any body byte",
			input:        "Content-Length: 5\r\n\r\n",
			wantCleanEOF: false,
		},
		{
			name:         "headers complete, stream ends mid body",
			input:        "Content-Length: 5\r\n\r\nab",
			wantCleanEOF: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := readFrame(bufio.NewReader(strings.NewReader(tt.input)))
			if err == nil {
				t.Fatalf("readFrame() error = nil, want an error")
			}
			if got := errors.Is(err, io.EOF); got != tt.wantCleanEOF {
				t.Fatalf("errors.Is(err, io.EOF) = %v, want %v (err = %v)", got, tt.wantCleanEOF, err)
			}
		})
	}
}

func truncate(b []byte) string {
	const maxLen = 200
	if len(b) <= maxLen {
		return string(b)
	}
	return fmt.Sprintf("%s...(%d more bytes)", b[:maxLen], len(b)-maxLen)
}

// testRand is a tiny deterministic xorshift PRNG for reproducible test
// chunk sizes, avoiding math/rand only to satisfy static analysis - the
// randomness quality is irrelevant here, determinism per seed is the point.
type testRand struct{ state uint64 }

func newTestRand(seed uint64) *testRand { return &testRand{state: seed*2654435761 + 1} }

// intn returns a deterministic value in [0, n). n must be positive.
func (r *testRand) intn(n int) int {
	if n <= 0 {
		return 0
	}
	r.state ^= r.state << 13
	r.state ^= r.state >> 7
	r.state ^= r.state << 17
	return int(r.state % uint64(n))
}
