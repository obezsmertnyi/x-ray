//go:build !cgo

package engine

// The CGO-free build must index doc-shaped files and must NOT
// destroy code nodes it cannot re-parse. Before this, UpdateByPaths refused
// every path outright; the danger in simply allowing it is that ParseOneFile
// returns zero nodes for code here, and Update reads zero nodes as "deleted".

import "testing"

func TestNeedsCodeParser(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"docs/adr/052.md", false},
		{"README.markdown", false},
		{"charts/xray/values.yaml", false},
		{"deploy/x.yml", false},
		{"db/releasescripts/7.0.0/create.SQL", false},
		{".env", false},
		{"deploy/.env.override", false},
		{"svc/src/main/resources/application-dev.yml", false},
		{"svc/src/main/resources/application.properties", false},
		{"svc/Dockerfile", false},
		{"src/frontend/Dockerfile.cypress", false},
		{"web/tsconfig.json", false},
		{"cmd/xray/main.go", true},
		{"web/app.ts", true},
		{"Makefile", true},
		{"UPPER.MD", false}, // extension check is case-insensitive
	} {
		if got := NeedsCodeParser(tc.path); got != tc.want {
			t.Errorf("NeedsCodeParser(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// parserAvailable must be false here, or the skip guard in Update never fires
// and code files get parsed to zero nodes and dropped.
func TestParserUnavailableInCGOFreeBuild(t *testing.T) {
	if parserAvailable {
		t.Fatal("parserAvailable must be false in a !cgo build")
	}
}
