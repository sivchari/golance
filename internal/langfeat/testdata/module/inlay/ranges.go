package inlay

func rangeOverMap() int {
	total := 0
	for k, v := range map[string]int{"a": 1, "b": 2} {
		total += len(k) + v
	}
	return total
}
