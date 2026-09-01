package embed

import "io"

// WithReader embeds io.Reader.
type WithReader interface {
	io.Reader
}
