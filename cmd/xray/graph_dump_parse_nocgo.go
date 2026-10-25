//go:build !cgo

package main

// CGO-free build: `dump <snapshot>` works (dump.go is untagged), but a
// repository target needs the parser, which is compiled out — same message as
// the other parse modes in parse_nocgo.go.

import "io"

func dumpRepo(io.Writer, string, dumpOpts) error { return errNeedsCGO }
