package xrayadk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"google.golang.org/adk/memory"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/triageagent-dev/x-ray/v2/pkg/xray"
)

// Memory is what the adapter needs from X-Ray. *xray.Client satisfies it, and a
// test can supply a fake.
type Memory interface {
	Search(ctx context.Context, query string, opts ...xray.QueryOption) ([]xray.Item, error)
	Upsert(ctx context.Context, w xray.Write) (*xray.WriteResult, error)
	Refute(ctx context.Context, id, reason string, evidence []string, opts ...xray.QueryOption) (*xray.RefuteResult, error)
}

// Service is an ADK memory.Service backed by X-Ray.
type Service struct {
	mem    Memory
	shared bool
	limit  int
}

var _ memory.Service = (*Service)(nil)

// Option configures New.
type Option func(*Service)

// WithoutShared limits a search to the user's own conversations, leaving out
// the shared memory other agents and people wrote.
func WithoutShared() Option { return func(s *Service) { s.shared = false } }

// WithLimit sets how many entries a search returns at most (default 10).
func WithLimit(n int) Option { return func(s *Service) { s.limit = n } }

// New returns a memory service over mem.
func New(mem Memory, opts ...Option) *Service {
	s := &Service{mem: mem, shared: true, limit: 10}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Subject is how X-Ray knows an app's user. Private memory is stored under it.
func Subject(appName, userID string) string { return appName + "/" + userID }

const maxExchangeChars = 2000

// AddSessionToMemory writes a session into X-Ray, one private note per
// exchange: a user turn and the agent's reply to it. Each note has a stable id,
// so adding the same session again updates it and adds only what is new.
func (s *Service) AddSessionToMemory(ctx context.Context, sess session.Session) error {
	subject := Subject(sess.AppName(), sess.UserID())
	for _, ex := range exchanges(sess) {
		_, err := s.mem.Upsert(ctx, xray.Write{
			ID:      noteID(sess, ex.firstEventID),
			Kind:    xray.KindNote,
			Scope:   xray.ScopePrivate,
			Subject: subject,
			Title:   title(ex.user),
			Content: fmt.Sprintf("User: %s\nAssistant: %s", ex.user, ex.reply),
			Tags:    []string{"adk", "session:" + sess.ID()},
		})
		if err != nil {
			return fmt.Errorf("xrayadk: save exchange: %w", err)
		}
	}
	return nil
}

// SearchMemory returns the user's own notes that match the query, then shared
// memory that does, best first.
func (s *Service) SearchMemory(ctx context.Context, req *memory.SearchRequest) (*memory.SearchResponse, error) {
	own, err := s.mem.Search(ctx, req.Query, xray.WithSubject(Subject(req.AppName, req.UserID)), xray.WithLimit(s.limit))
	if err != nil {
		return nil, fmt.Errorf("xrayadk: search: %w", err)
	}
	items := own
	if !s.shared {
		// A search for a subject can also return shared items that have no owner;
		// keep only what this user's conversations wrote.
		items = items[:0:0]
		for _, it := range own {
			if it.Scope == xray.ScopePrivate && it.Subject == Subject(req.AppName, req.UserID) {
				items = append(items, it)
			}
		}
	} else {
		shared, err := s.mem.Search(ctx, req.Query, xray.WithLimit(s.limit))
		if err != nil {
			return nil, fmt.Errorf("xrayadk: search shared: %w", err)
		}
		items = append(items, shared...)
	}
	resp := &memory.SearchResponse{}
	seen := map[string]bool{}
	for _, it := range items {
		if seen[it.ID] || len(resp.Memories) >= s.limit {
			continue
		}
		seen[it.ID] = true
		resp.Memories = append(resp.Memories, entry(it))
	}
	return resp, nil
}

func entry(it xray.Item) memory.Entry {
	text := it.Content
	if it.Title != "" && it.Kind != xray.KindNote {
		text = it.Title + ": " + it.Content
	}
	origin := it.Origin
	if origin == "" {
		origin = xray.OriginDerived
	}
	author := it.Subject
	if author == "" {
		author = "xray"
	}
	return memory.Entry{
		ID:        it.ID,
		Content:   genai.NewContentFromText(text, genai.RoleUser),
		Author:    author,
		Timestamp: it.UpdatedAt,
		CustomMetadata: map[string]any{
			"kind": it.Kind, "origin": origin, "scope": it.Scope, "xray_id": it.ID,
		},
	}
}

type exchange struct {
	firstEventID string
	user, reply  string
}

// exchanges groups a session's text events into user-turn and reply pairs.
// Function calls and partial streaming chunks carry no text and are skipped.
func exchanges(sess session.Session) []exchange {
	var out []exchange
	for ev := range sess.Events().All() {
		if ev.LLMResponse.Content == nil || ev.LLMResponse.Partial {
			continue
		}
		var parts []string
		for _, p := range ev.LLMResponse.Content.Parts {
			if p.Text != "" && !p.Thought {
				parts = append(parts, p.Text)
			}
		}
		if len(parts) == 0 {
			continue
		}
		text := strings.Join(parts, " ")
		if ev.Author == "user" {
			// A new user turn starts a new exchange; consecutive user turns join.
			if n := len(out); n > 0 && out[n-1].reply == "" {
				out[n-1].user = clip(out[n-1].user + " " + text)
				continue
			}
			out = append(out, exchange{firstEventID: ev.ID, user: clip(text)})
			continue
		}
		if n := len(out); n > 0 {
			out[n-1].reply = clip(strings.TrimSpace(out[n-1].reply + " " + text))
		}
	}
	// A user turn the agent has not answered yet is still worth keeping.
	return out
}

func clip(s string) string {
	if r := []rune(s); len(r) > maxExchangeChars {
		return string(r[:maxExchangeChars])
	}
	return s
}

func title(user string) string {
	r := []rune(strings.Join(strings.Fields(user), " "))
	if len(r) > 60 {
		return string(r[:60]) + "…"
	}
	return string(r)
}

// noteID is stable for one exchange of one session of one user, and contains
// nothing a caller could guess from another user's ids.
func noteID(sess session.Session, firstEventID string) string {
	sum := sha256.Sum256([]byte(sess.AppName() + "\x00" + sess.UserID() + "\x00" + sess.ID() + "\x00" + firstEventID))
	return "adk-" + hex.EncodeToString(sum[:12])
}
