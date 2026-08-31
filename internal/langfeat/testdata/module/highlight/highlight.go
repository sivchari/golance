package highlight

// Accumulate exercises document highlighting: total is declared, written
// to by the compound assignment, read by the return.
func Accumulate(values []int) int {
	total := 0
	for _, v := range values {
		total += v
	}
	return total
}
