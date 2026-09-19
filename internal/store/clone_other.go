//go:build !darwin

package store

import "errors"

// platformClone reports that no OS-specific copy-on-write clone
// implementation exists for this GOOS, so ClonePath always falls back to
// streamCopy here.
func platformClone(string, string) error {
	return errors.New("store: no platform clone implementation for this OS")
}
