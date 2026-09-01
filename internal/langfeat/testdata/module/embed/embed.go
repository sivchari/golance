package embed

// Base is embedded by Wrapper below.
type Base struct{}

// Wrapper embeds Base.
type Wrapper struct {
	Base
}
