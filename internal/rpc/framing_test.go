package rpc

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadFrame(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "basic",
			input: "Content-Length: 13\r\n\r\n{\"jsonrpc\":1}",
			want:  `{"jsonrpc":1}`,
		},
		{
			name:  "case insensitive header name",
			input: "content-length: 2\r\n\r\n{}",
			want:  "{}",
		},
		{
			name:  "extra headers ignored",
			input: "Content-Type: application/vscode-jsonrpc\r\nContent-Length: 2\r\n\r\n{}",
			want:  "{}",
		},
		{
			name:  "lf only line endings",
			input: "Content-Length: 2\n\n{}",
			want:  "{}",
		},
		{
			name:    "missing content-length",
			input:   "Content-Type: foo\r\n\r\n{}",
			wantErr: true,
		},
		{
			name:    "invalid content-length",
			input:   "Content-Length: abc\r\n\r\n{}",
			wantErr: true,
		},
		{
			name:    "truncated body",
			input:   "Content-Length: 10\r\n\r\n{}",
			wantErr: true,
		},
		{
			name:    "eof before headers",
			input:   "",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readFrame(bufio.NewReader(strings.NewReader(tt.input)))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("readFrame() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("readFrame() error = %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("readFrame() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadFrameMultipleMessages(t *testing.T) {
	input := "Content-Length: 2\r\n\r\n{}" + "Content-Length: 4\r\n\r\ntrue"
	r := bufio.NewReader(strings.NewReader(input))
	first, err := readFrame(r)
	if err != nil || string(first) != "{}" {
		t.Fatalf("first frame = %q, err = %v", first, err)
	}
	second, err := readFrame(r)
	if err != nil || string(second) != "true" {
		t.Fatalf("second frame = %q, err = %v", second, err)
	}
	if _, err := readFrame(r); !errors.Is(err, io.EOF) {
		t.Fatalf("third readFrame() error = %v, want io.EOF", err)
	}
}

func TestConnWrite(t *testing.T) {
	var buf bytes.Buffer
	c := newConn(&buf)
	if err := c.write([]byte(`{"a":1}`)); err != nil {
		t.Fatalf("write() error = %v", err)
	}
	want := "Content-Length: 7\r\n\r\n{\"a\":1}"
	if buf.String() != want {
		t.Fatalf("written frame = %q, want %q", buf.String(), want)
	}
}

func TestConnWriteRoundTripsThroughReadFrame(t *testing.T) {
	var buf bytes.Buffer
	c := newConn(&buf)
	body := []byte(`{"jsonrpc":"2.0","id":1,"result":null}`)
	if err := c.write(body); err != nil {
		t.Fatalf("write() error = %v", err)
	}
	got, err := readFrame(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("readFrame() error = %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("round-tripped frame = %q, want %q", got, body)
	}
}
