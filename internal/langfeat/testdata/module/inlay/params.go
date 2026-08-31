package inlay

func addNamed(x, y int) int { return x + y }

func sum3(prefix string, nums ...int) int {
	total := 0
	for _, n := range nums {
		total += n
	}
	return total
}

func paramCalls() int {
	x := 1
	y := 2
	matched := addNamed(x, y)
	swapped := addNamed(y, x)
	variadic := sum3("nums", 1, 2, 3)
	return matched + swapped + variadic
}
