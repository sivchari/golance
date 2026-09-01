package index

import (
	"encoding/binary"
	"hash/fnv"
	"testing"
)

// TestComputeUnitKey_FoldsInFactsSchemaVersion pins factsSchemaVersion's own
// purpose (see its doc): computeUnitKey's result must differ from what the
// pre-version-prefix computation (ownContentHash alone, no leading version
// bytes) would produce for the same content hash — otherwise bumping
// factsSchemaVersion on a facts-extraction/encoding change would not
// actually change any existing package's CAS key, and casHitOutcome would
// go on decoding old-format blobs the schema bump was supposed to make
// unreachable.
func TestComputeUnitKey_FoldsInFactsSchemaVersion(t *testing.T) {
	const ownContentHash = 42

	got := computeUnitKey(ownContentHash, nil)

	h := fnv.New64a()
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], ownContentHash)
	_, _ = h.Write(buf[:])
	_, _ = h.Write([]byte{0})
	withoutVersionPrefix := h.Sum64()

	if got == withoutVersionPrefix {
		t.Error("computeUnitKey() did not change from the pre-version-prefix computation: factsSchemaVersion is not actually folded into the key")
	}
}

// TestComputeUnitKey_DeterministicForSameInputs is a sanity check
// alongside the version-folding pin above: two calls with identical inputs
// must still agree, since computeUnitKey's whole purpose is a stable,
// content-addressed key.
func TestComputeUnitKey_DeterministicForSameInputs(t *testing.T) {
	deps := []depExportEntry{{path: "a", exportHash: 1}, {path: "b", exportHash: 2}}
	first := computeUnitKey(7, deps)
	second := computeUnitKey(7, deps)
	if first != second {
		t.Errorf("computeUnitKey() = %d, %d for identical inputs, want equal", first, second)
	}
}
