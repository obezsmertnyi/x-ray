package main

// `xray graph keygen` — generate an age X25519 identity for encrypted
// snapshots, so nothing outside this binary is needed to protect a map.
//
// The file is written in the age-keygen format (two comment lines and the
// AGE-SECRET-KEY-1… line) so it is interchangeable with a key made by the age
// tool, and so `kubectl create secret generic … --from-file=identity=<file>` is
// the whole provisioning step. The public recipient goes to stdout: that is the
// half a map builder needs (-snapshot-recipient), and the half that is safe to
// paste into a Makefile or a CI variable.
//
// Untagged: key handling is CGO-free and the static, snapshot-serving build is
// the one that runs where the key lives.

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"filippo.io/age"
)

func runKeygen(argv []string) error {
	return runKeygenTo(os.Stdout, os.Stderr, argv)
}

// runKeygenTo is runKeygen with the streams injected for tests.
func runKeygenTo(stdout, stderr io.Writer, argv []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "write the identity here (mode 0600); omitted = print the identity to stdout, like age-keygen")
	force := fs.Bool("force", false, "overwrite an existing -out file (default: refuse — a replaced key makes every snapshot encrypted to the old one unreadable)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: xray graph keygen [-out <identity file>] [-force]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	id, err := age.GenerateX25519Identity()
	if err != nil {
		return fmt.Errorf("generate identity: %w", err)
	}
	body := fmt.Sprintf("# created: %s\n# public key: %s\n%s\n",
		time.Now().UTC().Format(time.RFC3339), id.Recipient(), id)

	if *out == "" {
		_, err := io.WriteString(stdout, body)
		return err
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if !*force {
		flags |= os.O_EXCL
	}
	f, err := os.OpenFile(*out, flags, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%s exists; pass -force to replace it (snapshots encrypted to the old key become unreadable)", *out)
		}
		return err
	}
	if _, err := io.WriteString(f, body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// An identity file that pre-existed with looser permissions (-force) is
	// tightened too: the secret half is the only thing in it.
	if err := os.Chmod(*out, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "identity written to %s\n", *out)
	fmt.Fprintln(stdout, id.Recipient())
	return nil
}
