package engine

// Encrypted snapshots.
//
// Why: a snapshot is gzip over gob, and gob stores every string as plain
// bytes — `gunzip -c team.graph.gob.gz | strings` prints every node's text,
// which for a catalog map is real people's names, handles, roles and years of
// experience. Maps ship as OCI images, and a registry that anyone can pull from
// must not be a plaintext dump of the catalog. So a snapshot can be written
// through age (filippo.io/age: X25519 recipients, chunked ChaCha20-Poly1305,
// pure Go) and read back only by the holder of the matching identity.
//
// What it protects: the artifact at rest — on disk, on a PVC, in a registry.
// What it does not: the running server holds the graph in memory and answers
// MCP searches in the clear; access to the server is still the real boundary.
//
// Format detection is by sniffing, not by extension: an encrypted file starts
// with the age text header, a plain one with the gzip magic, and both keep the
// `<label>.graph.gob.gz` name. That keeps the chart's find/seed logic, the
// maps-image script and the directory watcher unchanged, and lets one directory
// hold both kinds during a migration.
//
// Keys are asymmetric on purpose: whoever builds a map needs only the public
// recipient (`age1…`), so a CI job or a laptop never holds the secret; the
// server holds the identity file.
//
// Write policy — "the key reads, the recipient writes" (WriteRecipient):
//
//   - -snapshot-key (identity) opens encrypted files and keeps them encrypted:
//     a map that was READ encrypted is flushed back to the identity's own
//     public key. It never encrypts anything else. A plaintext map stays
//     plaintext under a key, and so does a map the server creates (the
//     `remember` notes map).
//   - -snapshot-recipient encrypts every write: new maps, plaintext-loaded
//     maps (the only migration path) and encrypted-loaded ones alike.
//
// Why the identity does not imply a write recipient: Den's main server is one
// writable process that serves the team catalog encrypted while every other
// map — the session notes it flushes on every `remember` included — must stay
// plaintext. Under "identity ⇒ encrypt all flushes" that is impossible: the
// first note after the key is configured would encrypt the notes map. Tying
// encryption to what was on disk (plus an explicit recipient for opt-in) makes
// "this file is protected" a property of the file, decided once by whoever
// built it, not a side effect of which flags the server happened to start with.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"filippo.io/age"
)

// ageHeader is the first line of every age file; gzipMagic the first two
// bytes of every gzip stream. Sniffing one or the other decides the reader.
const ageHeader = "age-encryption.org/v1"

var gzipMagic = []byte{0x1f, 0x8b}

// snapshotKeyHint is the one sentence every "cannot read this" error carries,
// so an operator staring at a crashloop knows which flag, not which codec.
const snapshotKeyHint = "pass -snapshot-key <identity file> (or XRAY_SNAPSHOT_KEY)"

// ErrSnapshotEncrypted is returned (wrapped) when a snapshot is encrypted and
// no identity was configured, or none of the configured identities matches.
// Callers that register many maps errors.Is on it to skip-and-warn rather than
// fail the whole server.
var ErrSnapshotEncrypted = errors.New("snapshot is encrypted")

// SnapshotKeys is the optional key material snapshot reads and writes carry.
// The zero value means "no keys": plaintext files read and write as before,
// an encrypted file is refused with ErrSnapshotEncrypted.
type SnapshotKeys struct {
	identity  *age.X25519Identity // decrypts; re-encrypts files that were read encrypted
	recipient age.Recipient       // encrypts every write when set
	recipStr  string              // recipient as text, for logs
	keyPath   string              // where identity came from, for logs
}

// LoadSnapshotKeys reads an age identity file (identityPath, "" for none) and
// parses a recipient string (`age1…`, "" for none). Either may be empty; both
// empty yields the zero SnapshotKeys.
func LoadSnapshotKeys(identityPath, recipient string) (SnapshotKeys, error) {
	var k SnapshotKeys
	if identityPath != "" {
		id, err := readIdentityFile(identityPath)
		if err != nil {
			return SnapshotKeys{}, err
		}
		k.identity = id
		k.keyPath = identityPath
	}
	if recipient != "" {
		r, err := age.ParseX25519Recipient(strings.TrimSpace(recipient))
		if err != nil {
			return SnapshotKeys{}, fmt.Errorf("-snapshot-recipient: %w", err)
		}
		k.recipient = r
		k.recipStr = strings.TrimSpace(recipient)
	}
	return k, nil
}

// readIdentityFile parses the age-keygen file format: comment lines starting
// with '#', blank lines, and one AGE-SECRET-KEY-1… line. age.ParseIdentities
// would do the same but returns the interface, and the concrete X25519 identity
// is needed for Recipient() — a map read encrypted is re-encrypted to itself.
func readIdentityFile(path string) (*age.X25519Identity, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("-snapshot-key: %w", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		id, err := age.ParseX25519Identity(line)
		if err != nil {
			return nil, fmt.Errorf("-snapshot-key %s: %w", path, err)
		}
		return id, nil
	}
	return nil, fmt.Errorf("-snapshot-key %s: no AGE-SECRET-KEY-1 line (is this an age identity file?)", path)
}

// CanDecrypt reports whether an identity is loaded.
func (k SnapshotKeys) CanDecrypt() bool { return k.identity != nil }

// Encrypts reports whether writes of NEW or plaintext-origin files are
// encrypted — i.e. whether a recipient was named. An identity alone does not
// count: see WriteRecipient.
func (k SnapshotKeys) Encrypts() bool { return k.recipient != nil }

// Recipient is the explicit public key writes encrypt to, as text for logs;
// "" when no -snapshot-recipient was given.
func (k SnapshotKeys) Recipient() string { return k.recipStr }

// IdentityRecipient is the loaded identity's own public key, as text for logs;
// "" without an identity. It is what a file read encrypted is re-encrypted to.
func (k SnapshotKeys) IdentityRecipient() string {
	if k.identity == nil {
		return ""
	}
	return k.identity.Recipient().String()
}

// RecipientMismatch reports the one combination worth a startup warning: both
// an identity and a recipient are set, and they differ. Writes then go to a
// key this process cannot open, so after a restart the server cannot read
// what it wrote. Not refused — an operator rotating keys wants exactly that,
// briefly — but it must be visible in the log.
func (k SnapshotKeys) RecipientMismatch() bool {
	return k.identity != nil && k.recipient != nil && k.identity.Recipient().String() != k.recipStr
}

// KeyPath is where the identity was read from, for logs.
func (k SnapshotKeys) KeyPath() string { return k.keyPath }

// WriteRecipient decides how one write is protected. loadedEncrypted says
// whether the file being rewritten was encrypted when it was read (false for a
// new file). nil means plaintext. This is the single place the write policy
// lives — see the package comment for the rule and the incident behind it.
func (k SnapshotKeys) WriteRecipient(loadedEncrypted bool) age.Recipient {
	if k.recipient != nil {
		return k.recipient // opt-in: everything written here is encrypted
	}
	if loadedEncrypted && k.identity != nil {
		return k.identity.Recipient() // never downgrade: keep it as it arrived
	}
	return nil
}

// sniffEncrypted reports whether the stream starts with the age header, without
// consuming it. It returns the buffered reader that must be used from here on.
func sniffEncrypted(r io.Reader) (*bufio.Reader, bool, error) {
	br := bufio.NewReader(r)
	head, err := br.Peek(len(ageHeader))
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	if bytes.HasPrefix(head, []byte(ageHeader)) {
		return br, true, nil
	}
	if len(head) >= 2 && !bytes.HasPrefix(head, gzipMagic) {
		return nil, false, fmt.Errorf("not a snapshot: neither gzip nor age header (first bytes %q)", head[:min(len(head), 8)])
	}
	return br, false, nil
}

// IsEncryptedSnapshot sniffs the file at path. It reads a handful of bytes and
// is safe to call for every file at registration time.
func IsEncryptedSnapshot(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	_, enc, err := sniffEncrypted(f)
	return enc, err
}

// IsEncryptedSnapshotBytes is IsEncryptedSnapshot for an in-memory blob.
func IsEncryptedSnapshotBytes(b []byte) bool {
	return bytes.HasPrefix(b, []byte(ageHeader))
}

// openSnapshotStream turns the raw file stream into the gzip stream the gob
// decoders expect: pass-through for a plain file, age.Decrypt for an encrypted
// one. src names the snapshot in errors.
func openSnapshotStream(r io.Reader, keys SnapshotKeys, src string) (io.Reader, error) {
	br, enc, err := sniffEncrypted(r)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", src, err)
	}
	if !enc {
		return br, nil
	}
	if keys.identity == nil {
		return nil, fmt.Errorf("%s: %w; %s", src, ErrSnapshotEncrypted, snapshotKeyHint)
	}
	plain, err := age.Decrypt(br, keys.identity)
	if err != nil {
		var noMatch *age.NoIdentityMatchError
		if errors.As(err, &noMatch) {
			return nil, fmt.Errorf("%s: %w for a different key than %s; %s", src, ErrSnapshotEncrypted, keys.keyPath, snapshotKeyHint)
		}
		return nil, fmt.Errorf("%s: decrypt: %w", src, err)
	}
	return plain, nil
}
