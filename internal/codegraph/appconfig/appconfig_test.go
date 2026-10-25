package appconfig

import (
	"testing"
)

func keys(t *testing.T, path, src string) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, n := range Parse(path, []byte(src)).Nodes {
		if n.Kind != KindConfig {
			t.Fatalf("kind %q", n.Kind)
		}
		got[n.Name] = n.Body
	}
	return got
}

func TestParseProperties(t *testing.T) {
	got := keys(t, "application.properties", "# c\nserver.servlet.context-path=/api\nfeign.url: http://x/y\n")
	if got["server.servlet.context-path"] != "/api" || got["feign.url"] != "http://x/y" {
		t.Fatalf("got %v", got)
	}
}

func TestParseTSConfig_PathsWithCommentsAndTrailingCommas(t *testing.T) {
	got := keys(t, "web/tsconfig.json", `{
  // Next.js
  "compilerOptions": {
    "baseUrl": ".", /* root */
    "paths": { "@/*": ["./src/*",], "lib/*": ["../lib/*"] },
    "strict": true,
  },
  "include": ["**/*.ts"]
}`)
	for k, v := range map[string]string{
		"compilerOptions.baseUrl":     ".",
		"compilerOptions.paths.@/*":   "./src/*",
		"compilerOptions.paths.lib/*": "../lib/*",
		"compilerOptions.strict":      "true",
		"include":                     "**/*.ts",
	} {
		if got[k] != v {
			t.Errorf("%s = %q, want %q (all %v)", k, got[k], v, got)
		}
	}
	if !IsTSConfig("a/tsconfig.app.json") || !IsTSConfig("jsconfig.json") || IsTSConfig("package.json") {
		t.Error("IsTSConfig")
	}
}
