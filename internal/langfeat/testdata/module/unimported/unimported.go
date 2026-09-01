package unimported

import "strings"

func packagePrefixSite() {
	var _ = fm
}

func selectorSite() string {
	return fmt.Sp
}

func importedSelectorSite() string {
	return strings.To
}

type box struct{ v int }

func resolvedSelectorSite(b box) int {
	return b.v
}
