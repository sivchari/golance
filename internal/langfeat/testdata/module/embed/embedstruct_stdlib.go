package embed

import "bytes"

// WithBuffer embeds bytes.Buffer.
type WithBuffer struct {
	bytes.Buffer
}
