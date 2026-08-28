package store

import (
	"fmt"
	"math"
	"testing"
)

// benchRand is a small, fast, deterministic PRNG (splitmix64) for
// generating synthetic benchmark corpora — reproducible across runs from a
// fixed seed, without depending on math/rand's specific algorithm (which is
// free to change between Go versions) or math/rand's non-cryptographic
// randomness, which is irrelevant here: this is deterministic benchmark
// data generation, not a security-sensitive use.
type benchRand struct{ state uint64 }

func newBenchRand(seed uint64) *benchRand { return &benchRand{state: seed} }

// next returns the generator's next uint64.
func (r *benchRand) next() uint64 {
	r.state += 0x9E3779B97F4A7C15
	z := r.state
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

// intn returns a deterministic pseudo-random int in [0, n).
func (r *benchRand) intn(n int) int {
	if n <= 0 {
		return 0
	}
	v := r.next() % uint64(n)
	if v > math.MaxInt {
		return 0
	}
	return int(v)
}

// syntheticPackage holds one package's inputs plus enough bookkeeping to
// drive lookups against it once encoded and stored.
type syntheticPackage struct {
	pkgHash     uint64
	symIDHashes []uint64
	builder     *Builder
}

// generateCorpus builds numPkgs synthetic packages with symbolsPerPkg
// symbols and refsPerPkg refs each, deterministically (fixed seed) so
// benchmark runs are reproducible.
func generateCorpus(numPkgs, symbolsPerPkg, refsPerPkg int) []syntheticPackage {
	rng := newBenchRand(1)
	corpus := make([]syntheticPackage, numPkgs)
	for p := 0; p < numPkgs; p++ {
		pkgPath := fmt.Sprintf("example.com/corpus/pkg%d", p)
		pkgHash := Hash(pkgPath)

		b := NewBuilder()
		files := []string{fmt.Sprintf("%s/file.go", pkgPath)}
		b.SetFiles(files)

		symIDHashes := make([]uint64, symbolsPerPkg)
		for s := 0; s < symbolsPerPkg; s++ {
			name := fmt.Sprintf("Symbol%d", s)
			id := Hash(BuildSymbolID(pkgPath, name))
			symIDHashes[s] = id
			b.AddSymbol(&SymbolInput{
				IDHash:  id,
				Kind:    uint8(s % 8),
				Name:    name,
				Doc:     fmt.Sprintf("%s does something useful for package %s.", name, pkgPath),
				Sig:     fmt.Sprintf("func %s(x int, y string) (int, error)", name),
				FileIdx: 0,
				Line:    uint32(s + 1),
				Col:     6,
			})
		}
		for r := 0; r < refsPerPkg; r++ {
			target := symIDHashes[rng.intn(symbolsPerPkg)]
			b.AddRef(RefInput{
				FileIdx:        0,
				Line:           uint32(rng.intn(symbolsPerPkg) + 1),
				Col:            uint32(rng.intn(40) + 1),
				EndCol:         uint32(rng.intn(40) + 41),
				ToSymbolIDHash: target,
				ToPkgHash:      pkgHash,
			})
		}

		corpus[p] = syntheticPackage{pkgHash: pkgHash, symIDHashes: symIDHashes, builder: b}
	}
	return corpus
}

const (
	benchNumPkgs       = 2000
	benchSymbolsPerPkg = 200
	benchRefsPerPkg    = 300
)

// BenchmarkFullBuild measures the time to encode and persist a
// benchNumPkgs-package corpus (~benchSymbolsPerPkg symbols each) as CAS
// blobs, the write path an index full build is expected to use.
func BenchmarkFullBuild(b *testing.B) {
	corpus := generateCorpus(benchNumPkgs, benchSymbolsPerPkg, benchRefsPerPkg)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cas, err := OpenCAS(b.TempDir())
		if err != nil {
			b.Fatal(err)
		}
		for _, pkg := range corpus {
			blob, err := pkg.builder.Build()
			if err != nil {
				b.Fatal(err)
			}
			unit := EncodeUnitBlob(&UnitBlob{Facts: blob})
			if err := cas.Put(pkg.pkgHash, unit); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// buildBenchCAS encodes and persists the given corpus once, returning the
// opened CAS. Used by benchmarks that measure query latency, not build time.
func buildBenchCAS(b *testing.B, corpus []syntheticPackage) *CAS {
	b.Helper()
	cas, err := OpenCAS(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	for _, pkg := range corpus {
		blob, err := pkg.builder.Build()
		if err != nil {
			b.Fatal(err)
		}
		unit := EncodeUnitBlob(&UnitBlob{Facts: blob})
		if err := cas.Put(pkg.pkgHash, unit); err != nil {
			b.Fatal(err)
		}
	}
	return cas
}

// BenchmarkRandomSymbolLookup measures end-to-end latency of a single
// symbol lookup (read a package's blob from the CAS, decode its envelope,
// wrap Facts in a View, LookupSymbol, read Name/Doc/Sig) picked at random
// from the corpus — the query shape behind hover/definition.
func BenchmarkRandomSymbolLookup(b *testing.B) {
	corpus := generateCorpus(benchNumPkgs, benchSymbolsPerPkg, benchRefsPerPkg)
	cas := buildBenchCAS(b, corpus)

	rng := newBenchRand(2)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pkg := corpus[rng.intn(len(corpus))]
		idHash := pkg.symIDHashes[rng.intn(len(pkg.symIDHashes))]

		blob, ok, err := cas.Get(pkg.pkgHash)
		if err != nil || !ok {
			b.Fatalf("Get(%d) = %v, %v, %v", pkg.pkgHash, blob != nil, ok, err)
		}
		u, err := DecodeUnitBlob(blob)
		if err != nil {
			b.Fatal(err)
		}
		v, err := NewView(u.Facts)
		if err != nil {
			b.Fatal(err)
		}
		sym, ok := v.LookupSymbol(idHash)
		if !ok {
			b.Fatalf("LookupSymbol(%d) not found", idHash)
		}
		_ = sym.Name()
		_ = sym.Doc()
		_ = sym.Sig()
	}
}

// BenchmarkRefsTo measures the cost of a reverse-reference scan (RefsTo)
// within one package's facts blob — the query shape behind references,
// restricted to a single package (the index layer fans this out across the
// import-closure of candidate packages).
func BenchmarkRefsTo(b *testing.B) {
	corpus := generateCorpus(benchNumPkgs, benchSymbolsPerPkg, benchRefsPerPkg)
	cas := buildBenchCAS(b, corpus)

	rng := newBenchRand(3)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pkg := corpus[rng.intn(len(corpus))]
		idHash := pkg.symIDHashes[rng.intn(len(pkg.symIDHashes))]

		blob, ok, err := cas.Get(pkg.pkgHash)
		if err != nil || !ok {
			b.Fatalf("Get(%d) = %v, %v, %v", pkg.pkgHash, blob != nil, ok, err)
		}
		u, err := DecodeUnitBlob(blob)
		if err != nil {
			b.Fatal(err)
		}
		v, err := NewView(u.Facts)
		if err != nil {
			b.Fatal(err)
		}
		_ = v.RefsTo(idHash)
	}
}
