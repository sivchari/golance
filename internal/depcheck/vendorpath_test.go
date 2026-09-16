package depcheck_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sivchari/golance/internal/depcheck"
	"github.com/sivchari/golance/internal/graph"
)

// TestGraphMetadataSource_ResolvesGorootVendoredStdlibImports is the
// regression pin for GraphMetadataSource.Package's "vendor/" + pkgPath
// fallback: net/http transitively reaches "net" and "crypto/tls", both of
// which, on a unix-like GOOS, import golang.org/x/{net,crypto} packages by
// their literal source text (e.g. net/cgo_unix.go's own
// `import "golang.org/x/net/dns/dnsmessage"`) — cmd/go resolves that
// reference to GOROOT/src/vendor/golang.org/x/net/dns/dnsmessage, so
// go/packages.Load's own reported import graph keys it under the
// "vendor/"-prefixed path, not the bare one written in net's own source.
// Without the fallback, every standard-library package reachable through
// net/http or crypto/tls (effectively any network-touching dependency
// closure) degrades to Incomplete for a reason that has nothing to do with
// any real type-identity problem — confirmed against a real corpus
// (github.com/aws/aws-sdk-go-v2/service/s3 and its own transitive net/http
// dependency) before this fix.
func TestGraphMetadataSource_ResolvesGorootVendoredStdlibImports(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("testdata", "vendorpath"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}
	meta := depcheck.NewGraphMetadataSource(snap)
	provider := depcheck.NewProvider(meta, depcheck.Options{})

	for _, path := range []string{"net", "net/http", "crypto/tls"} {
		cp, err := provider.Package(context.Background(), path)
		if err != nil {
			t.Fatalf("Package(%s): %v", path, err)
		}
		if cp.Incomplete() {
			t.Errorf("Package(%s).Incomplete() = true, want false: FirstError=%q", path, cp.FirstError())
		}
	}
}
