// Command golance is a memory-bounded LSP server for Go. Run without
// arguments to serve LSP over stdio; see -h for flags.
package main

import "os"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
