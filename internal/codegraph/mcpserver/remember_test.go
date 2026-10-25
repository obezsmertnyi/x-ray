package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"
)

// fakeMemory records the one call remember makes, so a test can assert what
// the tool handed the backing store.
type fakeMemory struct {
	label  string
	note   Note
	forgot string
}

func (f *fakeMemory) Remember(_ context.Context, label string, n Note) (Remembered, error) {
	f.label, f.note = label, n
	r := Remembered{Map: label, QN: label + "//memory/x.md::x", At: time.Date(2026, 9, 15, 19, 0, 0, 0, time.UTC), Notes: 3}
	if n.TTL >= 0 {
		ttl := n.TTL
		if ttl == 0 {
			ttl = DefaultMemoryTTL
		}
		r.Expires = r.At.Add(ttl)
	}
	return r, nil
}

func (f *fakeMemory) Forget(_ context.Context, label, qn string) (Forgotten, error) {
	f.label, f.forgot = label, qn
	return Forgotten{Map: label, QN: qn, Notes: 2}, nil
}

func TestRemember_WritesOnlyToTheNotesMap(t *testing.T) {
	fm := &fakeMemory{}
	d := Deps{Memory: fm, MemoryMap: "session"}
	out, err := remember(context.Background(), d, "A user asked for the most experienced student in academy", "academy question", map[string]string{"topic": "academy"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if fm.label != "session" {
		t.Fatalf("every note must land in the server's notes map, got %q", fm.label)
	}
	if fm.note.Title != "academy question" || fm.note.Attrs["topic"] != "academy" || !strings.HasPrefix(fm.note.Text, "A user asked") {
		t.Fatalf("note not passed through intact: %+v", fm.note)
	}
	if out["map"] != "session" || out["notes"] != 3 || out["at"] != "2026-09-15T19:00:00Z" {
		t.Fatalf("reply shape: %v", out)
	}
	if out["expires"] != "2026-09-22T19:00:00Z" {
		t.Fatalf("a note with no ttl must report the default expiry (7d), got %v", out["expires"])
	}
	if n, _ := out["note"].(string); !strings.Contains(n, "recall") || !strings.Contains(n, "forget") {
		t.Fatalf("the reply must say how to read the note back and how to remove it, got %q", n)
	}
}

func TestRemember_Refusals(t *testing.T) {
	fm := &fakeMemory{}
	if _, err := remember(context.Background(), Deps{Memory: fm, MemoryMap: "session"}, "   ", "", nil, 0); err == nil {
		t.Fatal("an empty note must be refused before anything is written")
	}
	if _, err := remember(context.Background(), Deps{Memory: fm}, "x", "", nil, 0); err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("a Memory with no notes map is not enabled, got %v", err)
	}
	if _, err := RememberDirect(context.Background(), Deps{}, "x", "", nil, 0); err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("a server without memory must say so, got %v", err)
	}
}

func TestRememberDirect_ReturnsTheSameJSON(t *testing.T) {
	fm := &fakeMemory{}
	s, err := RememberDirect(context.Background(), Deps{Memory: fm, MemoryMap: "session"}, "note", "", nil, TTLNever)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(s), &got); err != nil {
		t.Fatal(err)
	}
	if got["qn"] != "session//memory/x.md::x" || got["notes"] != float64(3) || got["expires"] != "never" {
		t.Fatalf("direct twin must carry the same fields: %v", got)
	}
}

func TestParseAttrs(t *testing.T) {
	got, err := parseAttrs(" a=1, b = two ,, ")
	if err != nil || got["a"] != "1" || got["b"] != "two" || len(got) != 2 {
		t.Fatalf("parseAttrs: got %v err %v", got, err)
	}
	if got, err := parseAttrs(""); err != nil || got != nil {
		t.Fatalf("empty attrs must be nil, got %v err %v", got, err)
	}
	if _, err := parseAttrs("bad"); err == nil {
		t.Fatal("a pair without = must be an error, not a silent skip")
	}
	if _, err := parseAttrs("=v"); err == nil {
		t.Fatal("an empty key must be an error")
	}
}

// The tool is offered only when a store AND a notes map back it: a server
// with no memory must not list a remember that would fail, and the tool must
// carry no map argument — the caller never chooses where a note goes.
func TestRegisterRemember_OnlyWithMemoryAndANotesMap(t *testing.T) {
	without := server.NewMCPServer("t", "0")
	registerRemember(without, Deps{})
	if _, ok := without.ListTools()["remember"]; ok {
		t.Fatal("remember registered without a Memory backing")
	}
	noMap := server.NewMCPServer("t", "0")
	registerRemember(noMap, Deps{Memory: &fakeMemory{}})
	if _, ok := noMap.ListTools()["remember"]; ok {
		t.Fatal("remember registered without a notes map")
	}
	with := server.NewMCPServer("t", "0")
	registerRemember(with, Deps{Memory: &fakeMemory{}, MemoryMap: "session"})
	tool, ok := with.ListTools()["remember"]
	if !ok {
		t.Fatal("remember missing although Memory and MemoryMap are set")
	}
	if !strings.Contains(tool.Tool.Description, `"session"`) {
		t.Fatalf("the description must name the notes map, got %q", tool.Tool.Description)
	}
	if _, has := tool.Tool.InputSchema.Properties["map"]; has {
		t.Fatal("remember must not take a map argument")
	}
	for _, want := range []string{"text", "title", "attrs", "ttl"} {
		if _, has := tool.Tool.InputSchema.Properties[want]; !has {
			t.Errorf("remember must take %q", want)
		}
	}
	if !strings.Contains(tool.Tool.Description, "7d") || !strings.Contains(tool.Tool.Description, "recall") {
		t.Fatalf("the description must state the default lifetime and point at recall, got %q", tool.Tool.Description)
	}
	// forget registers alongside, under the same gate, and only takes a qn.
	if _, ok := without.ListTools()["forget"]; ok {
		t.Fatal("forget registered without a Memory backing")
	}
	ft, ok := with.ListTools()["forget"]
	if !ok {
		t.Fatal("forget missing although Memory and MemoryMap are set")
	}
	if _, has := ft.Tool.InputSchema.Properties["qn"]; !has || len(ft.Tool.InputSchema.Properties) != 1 {
		t.Fatalf("forget must take qn and nothing else, got %v", ft.Tool.InputSchema.Properties)
	}
}

func TestForget_GoesToTheNotesMapByQN(t *testing.T) {
	fm := &fakeMemory{}
	d := Deps{Memory: fm, MemoryMap: "session"}
	out, err := forget(context.Background(), d, " session//memory/x.md::x ")
	if err != nil {
		t.Fatal(err)
	}
	if fm.label != "session" || fm.forgot != "session//memory/x.md::x" {
		t.Fatalf("forget must hand the trimmed qn to the notes map, got %q in %q", fm.forgot, fm.label)
	}
	if out["map"] != "session" || out["qn"] != "session//memory/x.md::x" || out["notes"] != 2 {
		t.Fatalf("reply shape: %v", out)
	}
	if _, err := forget(context.Background(), d, "  "); err == nil {
		t.Fatal("an empty qn must be refused before anything is removed")
	}
	if _, err := ForgetDirect(context.Background(), Deps{}, "x"); err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("a server without memory must say so, got %v", err)
	}
	s, err := ForgetDirect(context.Background(), d, "session//memory/x.md::x")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(s), &got); err != nil {
		t.Fatal(err)
	}
	if got["notes"] != float64(2) {
		t.Fatalf("direct twin must carry the same fields: %v", got)
	}
}
