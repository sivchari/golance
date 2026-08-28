package symbols

// Widget has a name.
type Widget struct {
	Name string
}

// NewWidget returns a Widget named name.
func NewWidget(name string) *Widget {
	return &Widget{Name: name}
}

// Describe returns w's name.
func (w *Widget) Describe() string {
	return w.Name
}

// TopLevel is not a method.
func TopLevel() int {
	return 1
}

// Count is a package-level variable.
var Count = 0

// MaxWidgets is a package-level constant.
const MaxWidgets = 10
