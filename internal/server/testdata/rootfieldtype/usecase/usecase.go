package usecase

import "example.com/rootfieldtype/repository"

type U struct {
	s repository.Store
}

func (u *U) Call() string {
	return u.s.Get(1)
}
