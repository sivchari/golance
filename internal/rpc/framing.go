package rpc

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// errContextBytes caps how much of a frame's raw header bytes readFrame
// retains for its error messages, so a truncated, desynced, or malformed
// stream can be diagnosed from server logs — the classic first symptom is
// otherwise just "missing Content-Length header" with nothing to say what
// was actually on the wire — without risking an unbounded log line no
// matter how many header lines precede the failure.
const errContextBytes = 200

// readFrame reads one Content-Length-delimited JSON-RPC message body from r.
//
// A genuinely clean end of stream — the peer closed its write side between
// frames, with nothing at all read yet for this call — returns exactly
// io.EOF, unwrapped, so Server.Serve can tell it apart from every other
// failure below and treat it as an ordinary session end. Every other EOF
// (mid header line, mid header block, or mid body) means a frame was
// truncated partway through, which is never a normal shutdown: those are
// reported as io.ErrUnexpectedEOF instead, so Serve's own
// errors.Is(err, io.EOF) check cannot mistake a truncated frame for one and
// silently drop it without logging anything.
func readFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	first := true
	var seen []byte // up to errContextBytes of the header block read so far, for error context
	for {
		line, err := r.ReadString('\n')
		seen = appendCapped(seen, line, errContextBytes)
		if err != nil {
			if errors.Is(err, io.EOF) {
				if first && line == "" {
					return nil, io.EOF
				}
				err = io.ErrUnexpectedEOF
			}
			return nil, fmt.Errorf("rpc: header: %w (read: %q)", err, seen)
		}
		first = false
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("rpc: invalid Content-Length header %q: %w", v, err)
		}
		length = n
	}
	if length < 0 {
		return nil, fmt.Errorf("rpc: missing Content-Length header (read: %q)", seen)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("rpc: body: want %d bytes: %w (headers: %q)", length, err, seen)
	}
	return body, nil
}

// appendCapped appends add to dst, truncating add so dst never grows past
// capBytes — used to bound how much of a frame's raw header bytes readFrame
// retains for an error message, regardless of how many header lines a
// malformed or desynced stream sends before EOF or the terminating blank
// line.
func appendCapped(dst []byte, add string, capBytes int) []byte {
	if len(dst) >= capBytes {
		return dst
	}
	remaining := capBytes - len(dst)
	if len(add) > remaining {
		add = add[:remaining]
	}
	return append(dst, add...)
}

// conn writes framed JSON-RPC messages to an underlying stream. Writes are
// serialized so concurrently running handlers can share one conn safely.
type conn struct {
	mu sync.Mutex
	w  *bufio.Writer
}

func newConn(w io.Writer) *conn {
	return &conn{w: bufio.NewWriterSize(w, 1<<20)}
}

func (c *conn) write(body []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := fmt.Fprintf(c.w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	if _, err := c.w.Write(body); err != nil {
		return err
	}
	return c.w.Flush()
}
