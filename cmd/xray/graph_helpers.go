package main

// Helpers shared by the recall subcommands. Deliberately UNTAGGED: the
// snapshot-serving, servicemap, hook-client and install modes are CGO-free, and
// keeping these next to the CGO-only `serve` would drag the whole recall CLI
// behind the cgo build tag (which is what used to break `CGO_ENABLED=0 go build
// ./...`).

import (
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/rs/zerolog"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/engine"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/httpguard"
)

// Snapshot key flags, shared by every subcommand that reads or writes a
// <label>.graph.gob.gz (see internal/engine/snapshotkeys.go for the model).
// Both default from the environment so a pod gets them from a Secret mount and
// an env var, never from an args list visible in `kubectl describe`.
const (
	envSnapshotKey       = "XRAY_SNAPSHOT_KEY"
	envSnapshotRecipient = "XRAY_SNAPSHOT_RECIPIENT"
)

// snapshotKeyFlag registers -snapshot-key on fs: the age identity file that
// opens encrypted snapshots. The key READS — it never encrypts a plaintext or
// new file; a map it read encrypted is re-encrypted to it on flush.
func snapshotKeyFlag(fs *flag.FlagSet) *string {
	return fs.String("snapshot-key", os.Getenv(envSnapshotKey), "age identity file (from `xray graph keygen`) that opens encrypted snapshots; maps read encrypted stay encrypted on flush, everything else stays plaintext. Default $"+envSnapshotKey)
}

// snapshotRecipientFlag registers -snapshot-recipient on fs: the public key
// every written snapshot is encrypted to. The recipient WRITES — it is the
// only thing that turns a plaintext or new map into an encrypted one, and it
// never holds the secret, which is the shape a map-building CI job wants.
func snapshotRecipientFlag(fs *flag.FlagSet) *string {
	return fs.String("snapshot-recipient", os.Getenv(envSnapshotRecipient), "age public key (age1…) to encrypt every written snapshot to; empty = plaintext for new and plaintext-loaded maps. Default $"+envSnapshotRecipient)
}

// Access flags, shared by both servers (see internal/httpguard). The token is
// read from a file named by a flag or $XRAY_AUTH_TOKEN_FILE, never passed as a
// value, for the same reason as the snapshot key: args show in `ps` and in
// `kubectl describe`. Clients (the update/status/build subcommands and the
// PostToolUse hook) send $XRAY_AUTH_TOKEN.
const (
	envAuthTokenFile = "XRAY_AUTH_TOKEN_FILE"
	envAuthToken     = "XRAY_AUTH_TOKEN"
)

type accessFlags struct {
	tokenFile, allowedHosts, allowedOrigins *string
}

func registerAccessFlags(fs *flag.FlagSet) accessFlags {
	return accessFlags{
		tokenFile:      fs.String("auth-token-file", os.Getenv(envAuthTokenFile), "file holding a bearer token (16+ characters, e.g. `openssl rand -hex 32`) required on every route but /healthz; the browser UI takes it once as /ui?token=… and keeps it in a cookie. Empty = no token. Default $"+envAuthTokenFile),
		allowedHosts:   fs.String("allowed-hosts", "", "comma-separated Host names to accept besides loopback; a loopback -addr accepts only localhost, 127.0.0.1 and ::1 (DNS-rebinding guard), and any entry here turns the check on for other addresses too"),
		allowedOrigins: fs.String("allowed-origins", "", "comma-separated browser origins (scheme://host[:port]) allowed to call the server besides its own; requests from any other origin are refused"),
	}
}

// config builds the access checks for addr, reading the token file at once so
// a missing or short token fails the start rather than the first request, and
// says at startup what an exposed server would otherwise leave unsaid: that
// nobody is asked for a credential.
func (a accessFlags) config(addr string, logger zerolog.Logger) (httpguard.Config, error) {
	cfg := httpguard.Config{Addr: addr, AllowedHosts: splitList(*a.allowedHosts), AllowedOrigins: splitList(*a.allowedOrigins)}
	if *a.tokenFile != "" {
		tok, err := httpguard.LoadToken(*a.tokenFile)
		if err != nil {
			return cfg, err
		}
		cfg.Token = tok
		logger.Info().Str("tokenFile", *a.tokenFile).Msg("access: bearer token required on every route but /healthz")
	} else if !httpguard.IsLoopback(addr) {
		logger.Warn().Str("addr", addr).Msg("access: listening beyond loopback with no -auth-token-file — anyone who can reach this address can read every map, and write with -writable")
	}
	return cfg, nil
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// loadSnapshotKeys turns the two flags into engine.SnapshotKeys.
func loadSnapshotKeys(keyPath, recipient string) (engine.SnapshotKeys, error) {
	return engine.LoadSnapshotKeys(keyPath, recipient)
}

// searchLimitParam reads an optional ?limit= override. Zero means "let the
// resolver apply its own default", so an absent or malformed value falls
// through to that single default+cap rather than a second one defined here.
func searchLimitParam(r *http.Request) int {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func newLogger(level string) zerolog.Logger {
	lvl, err := zerolog.ParseLevel(level)
	if err != nil {
		lvl = zerolog.InfoLevel
	}
	// Match the server's console format so logs from both modes of this one
	// binary line up when read together.
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMs
	return zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05.000"}).
		Level(lvl).With().Timestamp().Logger()
}
