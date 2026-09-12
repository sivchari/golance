package index

import "testing"

// TestGenTable_TryCommit_OlderGenerationDoesNotClobberNewer verifies the
// ordering guard genTable exists to provide: once a higher generation has
// committed a write for a package, a lower generation's later attempt must
// be rejected, exactly mirroring check.Engine's identical
// commitCache guard (see internal/check's
// TestEngine_Commit_OlderGenerationDoesNotClobberNewer).
func TestGenTable_TryCommit_OlderGenerationDoesNotClobberNewer(t *testing.T) {
	gt := genTableFor(openTestDB(t))
	const pkgHash = 42

	genOld := gt.nextGen()
	genNew := gt.nextGen()

	// The newer generation commits first (its Reindex call started later
	// but finished sooner).
	if !gt.tryCommit(pkgHash, genNew) {
		t.Fatal("tryCommit(genNew) rejected, want accepted (first commit for pkgHash)")
	}
	// The older generation's write arrives after; it must be rejected
	// rather than overwriting the newer commit.
	if gt.tryCommit(pkgHash, genOld) {
		t.Fatal("tryCommit(genOld) accepted, want rejected (a newer generation already committed)")
	}
}

// TestGenTable_TryCommit_IndependentPackagesDoNotInterfere verifies that
// tryCommit gates each package independently: a generation stale for one
// pkgHash must still be accepted for another it has never touched.
func TestGenTable_TryCommit_IndependentPackagesDoNotInterfere(t *testing.T) {
	gt := genTableFor(openTestDB(t))
	const pkgA, pkgB = 1, 2

	genOld := gt.nextGen()
	genNew := gt.nextGen()

	if !gt.tryCommit(pkgA, genNew) {
		t.Fatal("tryCommit(pkgA, genNew) rejected, want accepted")
	}
	if !gt.tryCommit(pkgB, genOld) {
		t.Fatal("tryCommit(pkgB, genOld) rejected, want accepted (pkgB has no prior commit)")
	}
}

// TestGenTableFor_SameDBReturnsSameTable verifies genTableFor returns the
// same *genTable for repeated calls against the same db, and a distinct one
// for a different db — the sharing that lets concurrent Reindex calls
// against one db's ordering state actually interact, without letting
// unrelated databases (e.g. different tests' own temp DBs) influence each
// other's generations.
func TestGenTableFor_SameDBReturnsSameTable(t *testing.T) {
	db1 := openTestDB(t)
	db2 := openTestDB(t)

	first := genTableFor(db1)
	second := genTableFor(db1)
	if first != second {
		t.Error("genTableFor(db1) returned different tables across calls")
	}
	if first == genTableFor(db2) {
		t.Error("genTableFor(db1) == genTableFor(db2), want distinct tables for distinct databases")
	}
}
