// Package c is imported by package b, itself imported by package a — see
// rootexport_test.go.
package c

// Item is a named type whose Field must survive decode through b's own
// export data, without c's own export data ever being separately decoded.
type Item struct{ Field int }
