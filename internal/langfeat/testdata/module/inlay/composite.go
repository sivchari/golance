package inlay

type Point struct {
	X, Y int
}

func unkeyedPoint() Point {
	return Point{1, 2}
}

func pointSlice() []Point {
	return []Point{{1, 2}, {3, 4}}
}

func pointerSlice() []*Point {
	return []*Point{{1, 2}}
}
