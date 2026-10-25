package main

import (
	"embed"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// Embedded assets shipped inside the binary so `xray graph install` can drop
// the recall Claude Code skills + the file-watch hook into any repo, with no
// separate checkout. Keeping them in the binary means the guidance always
// matches the tool version that wrote it.
//
//go:embed assets/skills/*.md assets/hooks/*.sh
var assets embed.FS

// runInstall writes the embedded skills + hook into a target repo's .claude/ and
// prints the .mcp.json + settings.json snippets the user must add. It is
// non-destructive by default (won't overwrite existing files unless --force).
func runInstall(argv []string) error {
	fset := flag.NewFlagSet("install", flag.ExitOnError)
	dir := fset.String("dir", ".", "target repository root to install into")
	force := fset.Bool("force", false, "overwrite existing skill/hook files")
	_ = fset.Parse(argv)

	root, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	skillsDir := filepath.Join(root, ".claude", "skills")
	hooksDir := filepath.Join(root, ".claude", "xray", "hooks")

	wrote, skipped := 0, 0
	w, s, err := writeEmbedded("assets/skills", skillsDir, *force, 0o644)
	if err != nil {
		return err
	}
	wrote += w
	skipped += s
	w, s, err = writeEmbedded("assets/hooks", hooksDir, *force, 0o755)
	if err != nil {
		return err
	}
	wrote += w
	skipped += s

	fmt.Printf("xray graph install → %s\n", root)
	fmt.Printf("  skills: %s\n  hooks:  %s\n", skillsDir, hooksDir)
	fmt.Printf("  wrote %d file(s), skipped %d existing (use --force to overwrite)\n\n", wrote, skipped)
	printSnippets(root, hooksDir)
	return nil
}

// writeEmbedded copies every file under embedRoot into destDir, returning counts
// of written vs skipped (existing, when !force).
func writeEmbedded(embedRoot, destDir string, force bool, mode os.FileMode) (wrote, skipped int, err error) {
	if mkErr := os.MkdirAll(destDir, 0o755); mkErr != nil {
		return 0, 0, mkErr
	}
	entries, err := assets.ReadDir(embedRoot)
	if err != nil {
		return 0, 0, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, rErr := assets.ReadFile(embedRoot + "/" + e.Name())
		if rErr != nil {
			return wrote, skipped, rErr
		}
		dest := filepath.Join(destDir, e.Name())
		if !force {
			if _, statErr := os.Stat(dest); statErr == nil {
				skipped++
				continue
			}
		}
		if wErr := os.WriteFile(dest, data, mode); wErr != nil {
			return wrote, skipped, wErr
		}
		wrote++
	}
	return wrote, skipped, nil
}

// printSnippets shows the .mcp.json and settings.json hook config the user adds
// to wire the installed pieces (these are local config we don't write directly,
// since they may already exist and merging is the user's call).
func printSnippets(root, hooksDir string) {
	rel := func(p string) string {
		if r, err := filepath.Rel(root, p); err == nil {
			return r
		}
		return p
	}
	hookPath := filepath.Join(rel(hooksDir), "post-edit.sh")

	fmt.Println("Next steps — add to your repo config:")
	fmt.Println()
	fmt.Println("1) .mcp.json  → register the running server:")
	fmt.Println(`   "xray": { "type": "http", "url": "http://localhost:8084/mcp" }`)
	fmt.Println(`   (server started with -auth-token-file? add "headers": { "Authorization": "Bearer ${XRAY_AUTH_TOKEN}" })`)
	fmt.Println()
	fmt.Println("2) .claude/settings.json  → keep the graph fresh on edits/commits:")
	fmt.Printf(`   "PostToolUse": [{ "matcher": "Edit|Write|MultiEdit|Bash",
     "hooks": [{ "type": "command", "command": "bash \"$CLAUDE_PROJECT_DIR/%s\"", "timeout": 5000 }] }]`+"\n", hookPath)
	fmt.Println()
	fmt.Println("3) Start the server:  xray graph serve -repo <label>=<path>")
}
