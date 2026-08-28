package signature

// Add returns the sum of a and b.
func Add(a, b int) int {
	return a + b
}

// Sum returns the sum of nums.
func Sum(nums ...int) int {
	total := 0
	for _, n := range nums {
		total += n
	}
	return total
}

func callAdd() int {
	return Add(1, 2)
}

func callSum() int {
	return Sum(1, 2, 3, 4)
}
