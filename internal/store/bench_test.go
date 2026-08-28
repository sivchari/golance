package store

import (
	"fmt"
	"math/rand"
	"testing"
)

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
	rng := rand.New(rand.NewSource(1))
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
			b.AddSymbol(SymbolInput{
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
			target := symIDHashes[rng.Intn(symbolsPerPkg)]
			b.AddRef(RefInput{
				FileIdx:        0,
				Line:           uint32(rng.Intn(symbolsPerPkg) + 1),
				Col:            uint32(rng.Intn(40) + 1),
				EndCol:         uint32(rng.Intn(40) + 41),
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
			unit := EncodeUnitBlob(UnitBlob{Facts: blob})
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
		unit := EncodeUnitBlob(UnitBlob{Facts: blob})
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

	rng := rand.New(rand.NewSource(2))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pkg := corpus[rng.Intn(len(corpus))]
		idHash := pkg.symIDHashes[rng.Intn(len(pkg.symIDHashes))]

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

	rng := rand.New(rand.NewSource(3))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pkg := corpus[rng.Intn(len(corpus))]
		idHash := pkg.symIDHashes[rng.Intn(len(pkg.symIDHashes))]

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
