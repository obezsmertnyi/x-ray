package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/engine"
)

func TestKeygen_WritesIdentityFileAndPrintsRecipient(t *testing.T) {
	out := filepath.Join(t.TempDir(), "snapshot.key")
	var stdout, stderr bytes.Buffer
	if err := runKeygenTo(&stdout, &stderr, []string{"-out", out}); err != nil {
		t.Fatal(err)
	}
	recip := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(recip, "age1") {
		t.Fatalf("stdout must be the recipient, got %q", stdout.String())
	}

	st, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o, want 0600", st.Mode().Perm())
	}
	b, _ := os.ReadFile(out)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "# created: ") || !strings.HasPrefix(lines[1], "# public key: "+recip) || !strings.HasPrefix(lines[2], "AGE-SECRET-KEY-1") {
		t.Fatalf("unexpected identity file format:\n%s", b)
	}

	// The file loads as snapshot keys and encrypts to the printed recipient.
	keys, err := engine.LoadSnapshotKeys(out, "")
	if err != nil {
		t.Fatal(err)
	}
	if keys.IdentityRecipient() != recip {
		t.Fatalf("loaded identity's recipient %q != printed %q", keys.IdentityRecipient(), recip)
	}

	// Refuses to overwrite unless forced.
	if err := runKeygenTo(&stdout, &stderr, []string{"-out", out}); err == nil || !strings.Contains(err.Error(), "-force") {
		t.Fatalf("second keygen must refuse with a -force hint, got %v", err)
	}
	if err := runKeygenTo(&stdout, &stderr, []string{"-out", out, "-force"}); err != nil {
		t.Fatalf("-force: %v", err)
	}
	b2, _ := os.ReadFile(out)
	if bytes.Equal(b, b2) {
		t.Fatal("-force did not replace the identity")
	}
}

func TestKeygen_StdoutWhenNoOut(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := runKeygenTo(&stdout, &stderr, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "AGE-SECRET-KEY-1") || !strings.Contains(stdout.String(), "# public key: age1") {
		t.Fatalf("identity must be printed to stdout, got %q", stdout.String())
	}
	if err := runKeygenTo(&stdout, &stderr, []string{"stray"}); err == nil {
		t.Fatal("positional argument must be rejected")
	}
}

// dump reads an encrypted snapshot with -snapshot-key and refuses without it,
// naming the flag rather than failing on a codec.
func TestDump_EncryptedSnapshotNeedsKey(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "snapshot.key")
	var kout, kerr bytes.Buffer
	if err := runKeygenTo(&kout, &kerr, []string{"-out", keyFile}); err != nil {
		t.Fatal(err)
	}
	// The key reads, the recipient writes: to produce the encrypted fixture the
	// test encrypts to the identity's own public key, as a builder would.
	ro, err := engine.LoadSnapshotKeys(keyFile, "")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := engine.LoadSnapshotKeys(keyFile, ro.IdentityRecipient())
	if err != nil {
		t.Fatal(err)
	}
	plain := writeSnapshotFixture(t)
	snap, err := engine.ReadSnapshot(plain)
	if err != nil {
		t.Fatal(err)
	}
	enc := filepath.Join(t.TempDir(), "enc.graph.gob.gz")
	if err := engine.WriteSnapshotWith(enc, snap, keys); err != nil {
		t.Fatal(err)
	}

	t.Setenv(envSnapshotKey, "") // the flag's default must not leak in from the caller's shell
	_, _, err = runDumpForTest(t, enc, "-header")
	if err == nil || !strings.Contains(err.Error(), "-snapshot-key") {
		t.Fatalf("without a key: want an error naming -snapshot-key, got %v", err)
	}

	out, _, err := runDumpForTest(t, enc, "-header", "-snapshot-key", keyFile)
	if err != nil {
		t.Fatalf("with key: %v", err)
	}
	if !strings.Contains(out, "label:          fix") {
		t.Fatalf("header output: %q", out)
	}
	out, _, err = runVectorsForTest(t, enc, "-grep", "pear", "-snapshot-key", keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Pear") || !strings.Contains(out, "shown=1 of 3") {
		t.Fatalf("dump -vectors output: %q", out)
	}

	// The environment fallback works the same as the flag.
	t.Setenv(envSnapshotKey, keyFile)
	if _, _, err := runDumpForTest(t, enc, "-header"); err != nil {
		t.Fatalf("XRAY_SNAPSHOT_KEY fallback: %v", err)
	}
}
