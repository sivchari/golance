package rpc

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// readFrame reads one Content-Length-delimited JSON-RPC message body from r.
func readFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
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
		return nil, fmt.Errorf("rpc: missing Content-Length header")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
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
