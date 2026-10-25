//go:build !cgo

package main

// CGO-free build: the Tree-sitter parser is compiled out (internal/recall/parser
// is a stub), so the modes that PARSE source are unavailable. Everything that
// only READS a pre-built graph — serve-snapshots, servicemap, the hook clients,
// install, `dump <snapshot>` — is present and unaffected (a repository target
// for dump is stubbed in dump_parse_nocgo.go).
//
// These stubs exist so the static binary fails with an explanation instead of
// "unknown recall subcommand", which would read like the feature was dropped.

import "errors"

var errNeedsCGO = errors.New(
	"this build has no Tree-sitter parser: rebuild with CGO_ENABLED=1, " +
		"or serve a pre-built graph with `xray graph serve-snapshots`")

func runServe(argv []string) error    { return errNeedsCGO }
func runSnapshot(argv []string) error { return errNeedsCGO }
