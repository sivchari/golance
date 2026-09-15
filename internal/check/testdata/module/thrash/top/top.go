// Package top is TestEngine_Get_RootImportCacheDoesNotThrashUnderDefaultMaxLRU's
// entry point: it imports leafa (which resolves shared first), then seven
// filler packages (each occupying a cache slot), then shared again directly
// — exercising the exact eviction-order Engine's cache must not thrash on.
package top

import (
	"example.com/checkmod/thrash/leafa"

	"example.com/checkmod/thrash/filler1"
	"example.com/checkmod/thrash/filler2"
	"example.com/checkmod/thrash/filler3"
	"example.com/checkmod/thrash/filler4"
	"example.com/checkmod/thrash/filler5"
	"example.com/checkmod/thrash/filler6"
	"example.com/checkmod/thrash/filler7"

	"example.com/checkmod/thrash/shared"
)

// Sum adds every imported package's Value.
func Sum() int {
	return leafa.Value() + filler1.Value() + filler2.Value() + filler3.Value() +
		filler4.Value() + filler5.Value() + filler6.Value() + filler7.Value() + shared.Value()
}
