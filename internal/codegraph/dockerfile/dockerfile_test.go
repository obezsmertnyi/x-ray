package dockerfile

import (
	"testing"
)

func TestIsDockerfile(t *testing.T) {
	for p, want := range map[string]bool{"Dockerfile": true, "src/frontend/Dockerfile.cypress": true, "x/app.Dockerfile": true, "dockerfile.go": false, ".dockerignore": false} {
		if IsDockerfile(p) != want {
			t.Errorf("%s: %v", p, !want)
		}
	}
}
