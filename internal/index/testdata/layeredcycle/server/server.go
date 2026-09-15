// Package server exposes service over a transport, standing in for
// approvalagency's own server package: a downstream dependent of the
// usecase/infrastructure test-only cycle below.
package server

import (
	"example.com/layeredcycle/service"
	"example.com/layeredcycle/usecase"
)

// Handler wraps a usecase.Interactor obtained through service.New.
type Handler struct {
	interactor *usecase.Interactor
}

// NewHandler builds a Handler.
func NewHandler() *Handler {
	return &Handler{interactor: service.New()}
}

// Get delegates to the wrapped Interactor.
func (h *Handler) Get(id string) string {
	return h.interactor.Get(id).ID
}
