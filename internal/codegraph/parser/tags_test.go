//go:build cgo

package parser

import (
	"strings"
	"testing"
)

// TestExtractWithTags_CarriesBody pins that the generic Tree-sitter extractor
// stores each definition's full source in Body, the way the Go extractor does.
// Without it a TypeScript or Python symbol was findable but not readable:
// get_graph_node returned a bare header line, so the graph could locate code it
// could not show and every answer needed a file read to finish.
func TestExtractWithTags_CarriesBody(t *testing.T) {
	cases := []struct {
		name string
		path string
		src  string
		sym  string
		want []string
	}{
		{
			name: "typescript function",
			path: "web/src/goals.ts",
			src: "export function buildGoalsFacts(count: number): string {\n" +
				"  return `goal ${count}`;\n" +
				"}\n",
			sym:  "buildGoalsFacts",
			want: []string{"function buildGoalsFacts(count: number)", "return `goal ${count}`"},
		},
		{
			name: "python function",
			path: "svc/render.py",
			src: "def render_office(entity):\n" +
				"    return entity.name\n",
			sym:  "render_office",
			want: []string{"def render_office(entity)", "return entity.name"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := New().ParseFile(tc.path, []byte(tc.src))
			if err != nil {
				t.Fatalf("ParseFile: %v", err)
			}
			var got *NodeInfo
			for i := range res.Nodes {
				if res.Nodes[i].Name == tc.sym {
					got = &res.Nodes[i]
					break
				}
			}
			if got == nil {
				t.Fatalf("no node named %q in %d nodes", tc.sym, len(res.Nodes))
			}
			if got.Body == "" {
				t.Fatalf("%s: Body is empty — the tags extractor dropped the source", tc.sym)
			}
			for _, w := range tc.want {
				if !strings.Contains(got.Body, w) {
					t.Errorf("Body missing %q:\n%s", w, got.Body)
				}
			}
			// Body must describe the same span the node reports, or a reader
			// following file:line lands somewhere else.
			if lines := strings.Count(got.Body, "\n") + 1; lines > got.LineEnd-got.LineStart+1 {
				t.Errorf("Body spans %d lines, node reports %d-%d", lines, got.LineStart, got.LineEnd)
			}
		})
	}
}

// A Java call carries its receiver's declared type, and methods carry the class
// they belong to, so the linker can resolve calls the way it does for Go.
func TestExtractWithTags_JavaCallsCarryReceiverTypes(t *testing.T) {
	src := `package com.acme.a;

import com.acme.b.BClient;

public class AService {
    private final BClient client;
    private Helper helper;

    public String run(Request req) {
        Mapper m = new Mapper();
        for (Item it : req.items()) {
            it.check();
        }
        this.helper.help();
        m.map(req);
        Util.format("x");
        return client.fetch(req.id()) + local();
    }

    private String local() { return ""; }
}

class Other {
    String local() { return ""; }
}
`
	res, err := New().ParseFile("svc/src/main/java/com/acme/a/AService.java", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	parents := map[string]bool{}
	for _, n := range res.Nodes {
		if n.Kind == "Func" {
			parents[n.ParentName+"."+n.Name] = true
		}
	}
	for _, want := range []string{"AService.run", "AService.local", "Other.local"} {
		if !parents[want] {
			t.Errorf("no method %s in %v", want, parents)
		}
	}
	recv := map[string]string{}
	for _, e := range res.Edges {
		if e.Kind != EdgeCalls {
			continue
		}
		if e.Source != "svc/src/main/java/com/acme/a/AService.java::AService.run" {
			t.Errorf("call %s attributed to %s", e.Target, e.Source)
		}
		if e.Extra[ExtraJavaRecv] != "" {
			recv[e.Target] = e.Extra["recvType"]
		} else {
			recv[e.Target] = "var:" + e.Extra["recvVar"]
		}
	}
	want := map[string]string{
		"check":  "Item",
		"help":   "Helper",
		"map":    "Mapper",
		"format": "Util",
		"fetch":  "BClient",
		"local":  "AService",
		"items":  "Request",
		"id":     "Request",
	}
	for k, v := range want {
		if recv[k] != v {
			t.Errorf("%s: receiver %q, want %q (all: %v)", k, recv[k], v, recv)
		}
	}
}

// TypeScript and Python imports bind names, and calls carry the receiver the
// source declares, so the linker can resolve them.
func TestScriptFacts_ImportsAndReceivers(t *testing.T) {
	cases := []struct {
		path, src string
		imports   []string          // binding=module|imported
		recv      map[string]string // callName -> recvType / var:recvVar / bare / self:Type
		parents   []string
	}{
		{
			path: "web/src/svc.ts",
			src: `import Def, { A, B as C } from './lib/x';
import * as ns from '../y';
export class Svc {
  private readonly repo: Repo;
  constructor(private client: Client<T>) {}
  run(req: Req): void {
    const m = new Mapper();
    let z: Zed = make();
    this.repo.save(m);
    this.client.fetch();
    m.map(req); z.zap(); req.body(); ns.go(); A(); C.stat(); this.helper();
  }
  helper() {}
}
export function top() { const inner = () => 1; }
`,
			imports: []string{"Def=./lib/x|default", "A=./lib/x|A", "C=./lib/x|B", "ns=../y|*"},
			recv: map[string]string{
				"save": "Repo", "fetch": "Client", "map": "Mapper", "zap": "Zed", "body": "Req",
				"go": "var:ns", "A": "bare", "stat": "var:C", "helper": "self:Svc", "Mapper": "bare", "make": "bare",
			},
			parents: []string{"Svc.run", "Svc.helper", ".top", ".inner"},
		},
		{
			path: "svc/app/svc.py",
			src: `import os.path
import pkg.mod as m
from .sib import Helper, fn as f2
from ..up import *
class Svc(Base):
    def __init__(self, repo: Repo):
        self.repo = repo
        self.cli = Client()
    def run(self, req: "Req", n: int = 1) -> None:
        x = Mapper()
        y: Zed = make()
        self.repo.save(x)
        self.cli.get()
        x.map(req); y.zap(); req.body(); m.go(); f2(); Helper.build(); self.helper(); os.path.join("a")
        def nested():
            pass
`,
			imports: []string{"os.path=os.path|", "m=pkg.mod|", "Helper=.sib|Helper", "f2=.sib|fn", "*=..up|*"},
			recv: map[string]string{
				"save": "Repo", "get": "Client", "map": "Mapper", "zap": "Zed", "body": "Req",
				"go": "var:m", "f2": "bare", "build": "var:Helper", "helper": "self:Svc", "join": "var:os.path",
			},
			parents: []string{"Svc.__init__", "Svc.run", ".nested"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			res, err := New().ParseFile(tc.path, []byte(tc.src))
			if err != nil {
				t.Fatal(err)
			}
			var imports []string
			recv := map[string]string{}
			for _, e := range res.Edges {
				switch {
				case e.Kind == EdgeImports && e.Extra[ExtraScriptImport] != "":
					imports = append(imports, e.Extra[ExtraBinding]+"="+e.Target+"|"+e.Extra[ExtraImported])
				case e.Kind == EdgeCalls:
					v := ""
					switch {
					case e.Extra[ExtraSelfCall] != "":
						v = "self:" + e.Extra["recvType"]
					case e.Extra["recvType"] != "":
						v = e.Extra["recvType"]
					case e.Extra["bare"] != "":
						v = "bare"
					default:
						v = "var:" + e.Extra["recvVar"]
					}
					recv[e.Target] = v
				}
			}
			if strings.Join(imports, " ") != strings.Join(tc.imports, " ") {
				t.Errorf("imports = %v, want %v", imports, tc.imports)
			}
			for k, v := range tc.recv {
				if recv[k] != v {
					t.Errorf("%s: %q, want %q (all %v)", k, recv[k], v, recv)
				}
			}
			parents := map[string]bool{}
			for _, n := range res.Nodes {
				parents[n.ParentName+"."+n.Name] = true
			}
			for _, p := range tc.parents {
				if !parents[p] {
					t.Errorf("no def %s in %v", p, parents)
				}
			}
		})
	}
}

// TestExtractWithTags_KotlinAndSwift pins that Kotlin and Swift sources yield
// symbols the way the other tags-driven languages do.
func TestExtractWithTags_KotlinAndSwift(t *testing.T) {
	cases := []struct {
		name  string
		path  string
		src   string
		syms  []string
		calls []string
	}{
		{
			name: "kotlin",
			path: "app/Vitals.kt",
			src: "package app\n\n" +
				"class RingView(val goal: Int) {\n" +
				"    fun render(): String = format(goal)\n" +
				"}\n\n" +
				"object Defaults { fun weekly() = RingView(7).render() }\n\n" +
				"fun format(n: Int): String = n.toString()\n",
			syms:  []string{"RingView", "render", "Defaults", "weekly", "format"},
			calls: []string{"format", "render"},
		},
		{
			name: "swift",
			path: "App/Vitals.swift",
			src: "protocol Ringed { func render() -> String }\n\n" +
				"class RingView: Ringed {\n" +
				"    func render() -> String { return format(7) }\n" +
				"}\n\n" +
				"func format(_ n: Int) -> String { return String(n) }\n",
			syms:  []string{"Ringed", "RingView", "render", "format"},
			calls: []string{"format"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := New().ParseFile(tc.path, []byte(tc.src))
			if err != nil {
				t.Fatalf("ParseFile: %v", err)
			}
			have := map[string]bool{}
			for _, n := range res.Nodes {
				have[n.Name] = true
			}
			for _, s := range tc.syms {
				if !have[s] {
					t.Errorf("no node named %q; got %v", s, have)
				}
			}
			calls := map[string]bool{}
			for _, e := range res.Edges {
				if e.Kind == "calls" {
					calls[e.Target] = true
				}
			}
			for _, c := range tc.calls {
				if !calls[c] {
					t.Errorf("no call edge to %q; got %v", c, calls)
				}
			}
		})
	}
}
