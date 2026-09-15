// Package cmd stands in for approvalagency's own cmd/plugin and cmd/connect
// packages: several layers of production imports downstream of the
// usecase/infrastructure test-only cycle below.
package cmd

import "example.com/layeredcycle/server"

// Run builds a server.Handler and looks up id.
func Run(id string) string {
	return server.NewHandler().Get(id)
}
