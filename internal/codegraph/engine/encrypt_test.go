package engine

// Untagged on purpose: encrypted snapshots are served by the static build, so
// every path here must be proven under CGO_ENABLED=0 as well.

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/mcpserver"
)

// testKeys generates a fresh identity, writes it in age-keygen format, and
// returns keys that both read and write (identity + its own recipient) — what
// a builder or a migrating server runs with — plus the identity path and the
// recipient string. readOnlyKeys builds the identity-only variant from path.
func testKeys(t *testing.T) (SnapshotKeys, string, string) {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snapshot.key")
	body := "# created: test\n# public key: " + id.Recipient().String() + "\n" + id.String() + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := LoadSnapshotKeys(path, id.Recipient().String())
	if err != nil {
		t.Fatalf("LoadSnapshotKeys: %v", err)
	}
	return keys, path, id.Recipient().String()
}

// readOnlyKeys loads the identity at path with no recipient: it opens
// encrypted files and keeps them encrypted, and never encrypts anything else.
func readOnlyKeys(t *testing.T, path string) SnapshotKeys {
	t.Helper()
	k, err := LoadSnapshotKeys(path, "")
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// The marker is what must never appear in an encrypted file: a distinctive
// string planted in node text and in a vector-cache key.
const leakMarker = "octocat42-MARKER-7f3a"

func fixtureSnapshot() *Snapshot {
	return &Snapshot{
		Header: SnapshotHeader{
			Version:       snapshotVersion,
			Label:         "fix",
			RepoPath:      "/repo/fix",
			Fingerprint:   EmbedFingerprint{Model: "stub", Dims: 4, MaxInputChars: 512},
			BuiltAt:       time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC),
			Nodes:         1,
			Summary:       "fix: a fixture about " + leakMarker,
			SummaryVec:    []float32{1, 0, 0, 0},
			SummarySource: "explicit",
		},
		Body: SnapshotBody{
			Cache: map[string][]float32{"Student " + leakMarker + " 7 years": {0, 1, 0, 0}},
		},
	}
}

func TestEncryptedSnapshot_RoundTripEqualsPlaintext(t *testing.T) {
	keys, _, _ := testKeys(t)
	dir := t.TempDir()
	plain, enc := filepath.Join(dir, "plain.graph.gob.gz"), filepath.Join(dir, "enc.graph.gob.gz")
	if err := WriteSnapshot(plain, fixtureSnapshot()); err != nil {
		t.Fatal(err)
	}
	if err := WriteSnapshotWith(enc, fixtureSnapshot(), keys); err != nil {
		t.Fatal(err)
	}

	isEnc, err := IsEncryptedSnapshot(enc)
	if err != nil || !isEnc {
		t.Fatalf("IsEncryptedSnapshot(enc) = %v, %v; want true", isEnc, err)
	}
	if isEnc, _ := IsEncryptedSnapshot(plain); isEnc {
		t.Fatal("plaintext file reported as encrypted")
	}

	p, err := ReadSnapshotWith(plain, keys) // keys set, plaintext file: still readable
	if err != nil {
		t.Fatalf("plaintext with keys: %v", err)
	}
	e, err := ReadSnapshotWith(enc, keys)
	if err != nil {
		t.Fatalf("encrypted with keys: %v", err)
	}
	if p.Header.Summary != e.Header.Summary || len(p.Body.Cache) != len(e.Body.Cache) {
		t.Fatalf("round trips differ: %+v vs %+v", p.Header, e.Header)
	}
	for k, v := range p.Body.Cache {
		if got := e.Body.Cache[k]; len(got) != len(v) || got[1] != v[1] {
			t.Fatalf("cache entry %q differs: %v vs %v", k, got, v)
		}
	}

	// Header-only read stops after the first gob value on the encrypted file too.
	h, err := ReadSnapshotHeaderWith(enc, keys)
	if err != nil {
		t.Fatalf("ReadSnapshotHeaderWith: %v", err)
	}
	if h.Label != "fix" || h.SummarySource != "explicit" {
		t.Fatalf("header: %+v", h)
	}
}

func TestEncryptedSnapshot_NoPlaintextLeaks(t *testing.T) {
	keys, _, _ := testKeys(t)
	dir := t.TempDir()
	plain, enc := filepath.Join(dir, "plain.graph.gob.gz"), filepath.Join(dir, "enc.graph.gob.gz")
	if err := WriteSnapshot(plain, fixtureSnapshot()); err != nil {
		t.Fatal(err)
	}
	if err := WriteSnapshotWith(enc, fixtureSnapshot(), keys); err != nil {
		t.Fatal(err)
	}
	// gzip does not hide a string reliably but the marker is short and unique,
	// so the plaintext file is decompressed before searching — what `gunzip -c
	// | strings` does — and the test proves the marker IS there, so the
	// negative below means something.
	pb, err := ReadSnapshot(plain)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pb.Header.Summary, leakMarker) {
		t.Fatal("fixture lost its marker")
	}
	gunzipped := gunzipFile(t, plain)
	if !bytes.Contains(gunzipped, []byte(leakMarker)) {
		t.Fatal("plaintext snapshot does not contain the marker after gunzip — the leak test would prove nothing")
	}

	eb, err := os.ReadFile(enc)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(eb, []byte(leakMarker)) {
		t.Fatal("encrypted snapshot contains the marker in the clear")
	}
	if !bytes.HasPrefix(eb, []byte(ageHeader)) {
		t.Fatalf("encrypted file does not start with the age header: %q", eb[:24])
	}
}

func TestEncryptedSnapshot_RefusedWithoutOrWithWrongKey(t *testing.T) {
	keys, _, _ := testKeys(t)
	enc := filepath.Join(t.TempDir(), "enc.graph.gob.gz")
	if err := WriteSnapshotWith(enc, fixtureSnapshot(), keys); err != nil {
		t.Fatal(err)
	}

	for name, k := range map[string]SnapshotKeys{"no key": {}, "wrong key": func() SnapshotKeys { k, _, _ := testKeys(t); return k }()} {
		_, err := ReadSnapshotHeaderWith(enc, k)
		if err == nil {
			t.Fatalf("%s: encrypted header read must fail", name)
		}
		if !errors.Is(err, ErrSnapshotEncrypted) {
			t.Fatalf("%s: error must wrap ErrSnapshotEncrypted, got %v", name, err)
		}
		if !strings.Contains(err.Error(), "-snapshot-key") {
			t.Fatalf("%s: error must name the flag, got %v", name, err)
		}
		if _, err := ReadSnapshotWith(enc, k); err == nil || !errors.Is(err, ErrSnapshotEncrypted) {
			t.Fatalf("%s: full read must fail the same way, got %v", name, err)
		}
	}
}

func TestLoadSnapshotKeys(t *testing.T) {
	_, path, recip := testKeys(t)

	// The key reads, the recipient writes: an identity alone opens files and
	// re-encrypts only what arrived encrypted; it never encrypts a new file.
	k, err := LoadSnapshotKeys(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if !k.CanDecrypt() || k.Encrypts() || k.Recipient() != "" || k.IdentityRecipient() != recip {
		t.Fatalf("identity alone: decrypt=%v encrypts=%v recip=%q identityRecip=%q (want decrypt, no encrypt, own key %q)", k.CanDecrypt(), k.Encrypts(), k.Recipient(), k.IdentityRecipient(), recip)
	}
	if k.WriteRecipient(false) != nil {
		t.Fatal("identity alone must write a NEW file as plaintext")
	}
	if got := k.WriteRecipient(true); got == nil || got.(*age.X25519Recipient).String() != recip {
		t.Fatalf("identity alone must re-encrypt a file that was read encrypted to its own key, got %v", got)
	}
	if k.RecipientMismatch() {
		t.Fatal("no recipient set: nothing to mismatch")
	}

	_, _, otherRecip := testKeys(t)
	k2, err := LoadSnapshotKeys(path, otherRecip)
	if err != nil {
		t.Fatal(err)
	}
	if k2.Recipient() != otherRecip || !k2.Encrypts() {
		t.Fatalf("explicit recipient must be the write key: %q encrypts=%v", k2.Recipient(), k2.Encrypts())
	}
	if got := k2.WriteRecipient(true).(*age.X25519Recipient).String(); got != otherRecip {
		t.Fatalf("explicit recipient must win over the identity's own even for an encrypted-loaded file: %q", got)
	}
	if !k2.RecipientMismatch() {
		t.Fatal("identity and a different recipient must report a mismatch (writes this process cannot read back)")
	}
	same, err := LoadSnapshotKeys(path, recip)
	if err != nil {
		t.Fatal(err)
	}
	if same.RecipientMismatch() {
		t.Fatal("identity plus its own recipient is not a mismatch")
	}

	k3, err := LoadSnapshotKeys("", recip)
	if err != nil {
		t.Fatal(err)
	}
	if k3.CanDecrypt() || !k3.Encrypts() || k3.WriteRecipient(false) == nil {
		t.Fatalf("recipient alone: decrypt=%v encrypts=%v", k3.CanDecrypt(), k3.Encrypts())
	}

	var zero SnapshotKeys
	if zero.CanDecrypt() || zero.Encrypts() || zero.Recipient() != "" || zero.WriteRecipient(true) != nil {
		t.Fatal("zero keys must be plaintext-only")
	}

	if _, err := LoadSnapshotKeys("", "not-a-recipient"); err == nil {
		t.Fatal("garbage recipient must be rejected")
	}
	bad := filepath.Join(t.TempDir(), "bad.key")
	_ = os.WriteFile(bad, []byte("# just a comment\n"), 0o600)
	if _, err := LoadSnapshotKeys(bad, ""); err == nil || !strings.Contains(err.Error(), "AGE-SECRET-KEY-1") {
		t.Fatalf("identity file without a key line must be rejected with a hint, got %v", err)
	}
	if _, err := LoadSnapshotKeys(filepath.Join(t.TempDir(), "missing"), ""); err == nil {
		t.Fatal("missing identity file must be rejected")
	}
}

// A directory can hold both kinds: registration and load work for each.
func TestRegistry_MixedEncryptedAndPlaintextMaps(t *testing.T) {
	keys, _, _ := testKeys(t)
	emb := &stubEmbedder{dims: 32}
	fp := EmbedFingerprint{Model: "stub", Dims: emb.dims, MaxInputChars: 8192}
	dir := t.TempDir()

	// Build two notes maps through Remember: one registry with a recipient
	// writes an encrypted file, one without keys writes a plaintext one.
	rEnc := NewRegistry(emb, fp, 1, 0, true, testLogger())
	rEnc.SetSnapshotDir(dir)
	rEnc.SetSnapshotKeys(keys)
	if _, err := rEnc.Remember(context.Background(), "secretnotes", mcpserver.Note{Text: "alpha " + leakMarker}); err != nil {
		t.Fatal(err)
	}
	rPlain := NewRegistry(emb, fp, 1, 0, true, testLogger())
	rPlain.SetSnapshotDir(dir)
	if _, err := rPlain.Remember(context.Background(), "opennotes", mcpserver.Note{Text: "beta open note"}); err != nil {
		t.Fatal(err)
	}
	if enc, _ := IsEncryptedSnapshot(filepath.Join(dir, "secretnotes.graph.gob.gz")); !enc {
		t.Fatal("registry with a recipient must flush an encrypted file")
	}
	if enc, _ := IsEncryptedSnapshot(filepath.Join(dir, "opennotes.graph.gob.gz")); enc {
		t.Fatal("registry without keys must flush plaintext")
	}

	// A fresh server with the key registers and loads both.
	r := NewRegistry(emb, fp, 1, 0, false, testLogger())
	r.SetSnapshotKeys(keys)
	for _, f := range []string{"secretnotes", "opennotes"} {
		if err := r.AddSnapshot(filepath.Join(dir, f+".graph.gob.gz")); err != nil {
			t.Fatalf("AddSnapshot %s: %v", f, err)
		}
		res, err := r.ResolveMap(context.Background(), f)
		if err != nil {
			t.Fatalf("ResolveMap %s: %v", f, err)
		}
		if res.Graph.Stats().Nodes != 1 {
			t.Fatalf("%s: nodes=%d", f, res.Graph.Stats().Nodes)
		}
	}

	// Without the key the encrypted one is refused with the hint; the plain one still registers.
	rNoKey := NewRegistry(emb, fp, 1, 0, false, testLogger())
	err := rNoKey.AddSnapshot(filepath.Join(dir, "secretnotes.graph.gob.gz"))
	if err == nil || !errors.Is(err, ErrSnapshotEncrypted) || !strings.Contains(err.Error(), "-snapshot-key") {
		t.Fatalf("no key: want ErrSnapshotEncrypted with hint, got %v", err)
	}
	if err := rNoKey.AddSnapshot(filepath.Join(dir, "opennotes.graph.gob.gz")); err != nil {
		t.Fatalf("plaintext must still register without keys: %v", err)
	}
}

// A map that arrived encrypted stays encrypted across a restart even when the
// server has only the identity (no -snapshot-recipient): the flush re-encrypts
// to the identity's own key, and the file keeps loading afterwards.
func TestRegistry_WritableFlushStaysEncryptedAcrossRestart(t *testing.T) {
	keys, keyPath, _ := testKeys(t)
	emb := &stubEmbedder{dims: 32}
	fp := EmbedFingerprint{Model: "stub", Dims: emb.dims, MaxInputChars: 8192}
	dir := t.TempDir()
	path := filepath.Join(dir, "session.graph.gob.gz")

	r1 := NewRegistry(emb, fp, 1, 0, true, testLogger()) // builder-shaped: recipient set
	r1.SetSnapshotDir(dir)
	r1.SetSnapshotKeys(keys)
	if _, err := r1.Remember(context.Background(), "session", mcpserver.Note{Text: "first " + leakMarker, Title: "first"}); err != nil {
		t.Fatal(err)
	}
	if enc, _ := IsEncryptedSnapshot(path); !enc {
		t.Fatal("precondition: encrypted")
	}

	r2 := NewRegistry(emb, fp, 1, 0, true, testLogger()) // server-shaped: identity only
	r2.SetSnapshotDir(dir)
	r2.SetSnapshotKeys(readOnlyKeys(t, keyPath))
	if err := r2.AddSnapshot(path); err != nil {
		t.Fatal(err)
	}
	if _, err := r2.Remember(context.Background(), "session", mcpserver.Note{Text: "second note", Title: "second"}); err != nil {
		t.Fatal(err)
	}
	if enc, _ := IsEncryptedSnapshot(path); !enc {
		t.Fatal("flush after reload under an identity alone must stay encrypted")
	}
	if e := r2.entry("session"); e == nil || !e.encrypted {
		t.Fatal("entry must still remember it is encrypted")
	}
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte(leakMarker)) {
		t.Fatal("marker leaked after the second flush")
	}

	r3 := NewRegistry(emb, fp, 1, 0, false, testLogger())
	r3.SetSnapshotKeys(readOnlyKeys(t, keyPath))
	if err := r3.AddSnapshot(path); err != nil {
		t.Fatal(err)
	}
	res, err := r3.ResolveMap(context.Background(), "session")
	if err != nil {
		t.Fatal(err)
	}
	if res.Graph.Stats().Nodes != 2 {
		t.Fatalf("nodes=%d, want both notes", res.Graph.Stats().Nodes)
	}
}

// A key alone never encrypts a plaintext map: Den's main server serves the
// team catalog encrypted while its session notes must stay plaintext, so a
// plaintext map loaded under an identity is flushed plaintext. Migration to
// encrypted is opt-in through -snapshot-recipient.
func TestRegistry_PlaintextMapStaysPlaintextUnderKeyAlone(t *testing.T) {
	emb := &stubEmbedder{dims: 32}
	fp := EmbedFingerprint{Model: "stub", Dims: emb.dims, MaxInputChars: 8192}
	dir := t.TempDir()
	path := filepath.Join(dir, "session.graph.gob.gz")

	r0 := NewRegistry(emb, fp, 1, 0, true, testLogger())
	r0.SetSnapshotDir(dir)
	if _, err := r0.Remember(context.Background(), "session", mcpserver.Note{Text: "plain era", Title: "plain"}); err != nil {
		t.Fatal(err)
	}
	if enc, _ := IsEncryptedSnapshot(path); enc {
		t.Fatal("precondition: plaintext")
	}

	rw, keyPath, _ := testKeys(t)
	r1 := NewRegistry(emb, fp, 1, 0, true, testLogger())
	r1.SetSnapshotDir(dir)
	r1.SetSnapshotKeys(readOnlyKeys(t, keyPath))
	if err := r1.AddSnapshot(path); err != nil {
		t.Fatal(err)
	}
	if _, err := r1.Remember(context.Background(), "session", mcpserver.Note{Text: "still plain era", Title: "still plain"}); err != nil {
		t.Fatal(err)
	}
	if enc, _ := IsEncryptedSnapshot(path); enc {
		t.Fatal("a key alone must NOT encrypt a plaintext map on flush")
	}
	if e := r1.entry("session"); e == nil || e.encrypted {
		t.Fatal("entry must still be plaintext")
	}
	if _, err := ReadSnapshot(path); err != nil {
		t.Fatalf("file must stay readable without any key: %v", err)
	}

	// Opt-in migration: the same directory under a recipient encrypts on the next flush.
	// (Notes carry titles: two written in the same millisecond would otherwise share a name.)
	r2 := NewRegistry(emb, fp, 1, 0, true, testLogger())
	r2.SetSnapshotDir(dir)
	r2.SetSnapshotKeys(rw)
	if err := r2.AddSnapshot(path); err != nil {
		t.Fatal(err)
	}
	if _, err := r2.Remember(context.Background(), "session", mcpserver.Note{Text: "encrypted era", Title: "encrypted"}); err != nil {
		t.Fatal(err)
	}
	if enc, _ := IsEncryptedSnapshot(path); !enc {
		t.Fatal("first flush under a recipient must encrypt the file")
	}
	if e := r2.entry("session"); e == nil || !e.encrypted {
		t.Fatal("entry must now remember it is encrypted")
	}
	res, err := r2.ResolveMap(context.Background(), "session")
	if err != nil || res.Graph.Stats().Nodes != 3 {
		t.Fatalf("all three notes must survive the migration: %v nodes=%d", err, res.Graph.Stats().Nodes)
	}
}

// A notes map the server CREATES follows the same rule: plaintext under an
// identity alone, encrypted under a recipient.
func TestRegistry_CreatedNotesMapFollowsWritePolicy(t *testing.T) {
	emb := &stubEmbedder{dims: 32}
	fp := EmbedFingerprint{Model: "stub", Dims: emb.dims, MaxInputChars: 8192}
	rw, keyPath, _ := testKeys(t)

	dirRO := t.TempDir()
	rRO := NewRegistry(emb, fp, 1, 0, true, testLogger())
	rRO.SetSnapshotDir(dirRO)
	rRO.SetSnapshotKeys(readOnlyKeys(t, keyPath))
	if _, err := rRO.Remember(context.Background(), "session", mcpserver.Note{Text: "created under key alone"}); err != nil {
		t.Fatal(err)
	}
	if enc, _ := IsEncryptedSnapshot(filepath.Join(dirRO, "session.graph.gob.gz")); enc {
		t.Fatal("notes map created under an identity alone must be plaintext")
	}

	dirRW := t.TempDir()
	rRW := NewRegistry(emb, fp, 1, 0, true, testLogger())
	rRW.SetSnapshotDir(dirRW)
	rRW.SetSnapshotKeys(rw)
	if _, err := rRW.Remember(context.Background(), "session", mcpserver.Note{Text: "created under recipient " + leakMarker}); err != nil {
		t.Fatal(err)
	}
	encPath := filepath.Join(dirRW, "session.graph.gob.gz")
	if enc, _ := IsEncryptedSnapshot(encPath); !enc {
		t.Fatal("notes map created under a recipient must be encrypted")
	}
	raw, _ := os.ReadFile(encPath)
	if bytes.Contains(raw, []byte(leakMarker)) {
		t.Fatal("marker in the clear")
	}
}

// Never downgrade: an entry read encrypted refuses to flush once the keys are
// gone, rather than writing the catalog back in the clear.
func TestRegistry_FlushRefusesPlaintextDowngrade(t *testing.T) {
	keys, _, _ := testKeys(t)
	emb := &stubEmbedder{dims: 32}
	fp := EmbedFingerprint{Model: "stub", Dims: emb.dims, MaxInputChars: 8192}
	dir := t.TempDir()
	path := filepath.Join(dir, "session.graph.gob.gz")

	r := NewRegistry(emb, fp, 1, 0, true, testLogger())
	r.SetSnapshotDir(dir)
	r.SetSnapshotKeys(keys)
	if _, err := r.Remember(context.Background(), "session", mcpserver.Note{Text: "kept " + leakMarker}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)

	r.SetSnapshotKeys(SnapshotKeys{}) // keys removed from a running registry
	_, err := r.Remember(context.Background(), "session", mcpserver.Note{Text: "would downgrade"})
	if err == nil || !strings.Contains(err.Error(), "refusing to flush") {
		t.Fatalf("flush without keys after an encrypted read must refuse, got %v", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("file was rewritten despite the refusal")
	}
	if bytes.Contains(after, []byte(leakMarker)) {
		t.Fatal("marker in the clear")
	}
}

// gunzipFile returns the decompressed bytes of a plaintext snapshot — what
// `gunzip -c file | strings` would scan.
func gunzipFile(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(gz); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
