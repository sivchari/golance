// Package filler7 is one of several packages top imports between leafa and
// its own direct import of shared, each occupying one of Engine's cache
// slots — enough of them (more than defaultMaxLRU) evicts shared's own
// entry, added earlier, if Engine's cache is sized too small for the whole
// closure. See TestEngine_Get_RootImportCacheDoesNotThrashUnderDefaultMaxLRU.
package filler7

// Value returns a constant.
func Value() int { return 7 }
