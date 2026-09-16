package repository

type Store interface {
	Get(id int) string
}

type Impl struct{}

func (Impl) Get(id int) string { return "" }
