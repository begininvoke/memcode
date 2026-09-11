package permissions

import "testing"

// A denylist is a capability ceiling, so it must not be defeatable by shell
// syntax. The risk classifier is AST-based for the same reason; this rides the
// same walk.
func TestDeniedByResistsShellTricks(t *testing.T) {
	deny := []string{"git push", "gh"}
	for _, c := range []string{
		"git push",
		"git push -u origin auto/x",
		"/usr/bin/git push",
		"echo hi && git push",
		"eval 'git push'",
		"(cd /tmp && git push)",
		"true; git push origin main",
		"if true; then git push; fi",
		"gh pr create --title x",
		"GIT_DIR=. git push",
	} {
		if _, denied := DeniedBy(c, deny); !denied {
			t.Errorf("%q must be denied", c)
		}
	}
}

func TestDeniedByAllowsEverythingElse(t *testing.T) {
	deny := []string{"git push", "gh"}
	for _, c := range []string{
		"go test ./...",
		"go build ./...",
		"git status",
		"git commit -m x",
		"git log --oneline",
		"rg pattern",
		"cat file.txt",
		"echo 'git push is a string, not a command'",
	} {
		if m, denied := DeniedBy(c, deny); denied {
			t.Errorf("%q must be allowed, matched %q", c, m)
		}
	}
}

func TestDeniedByWildcards(t *testing.T) {
	if _, denied := DeniedBy("git push --force origin main", []string{"git * --force"}); !denied {
		t.Error("a wildcard token must match")
	}
	if _, denied := DeniedBy("git push origin main", []string{"git * --force"}); denied {
		t.Error("the wildcard must not match a different flag")
	}
}

// No patterns is no ceiling — the common case must be free.
func TestDeniedByEmptyPatterns(t *testing.T) {
	if _, denied := DeniedBy("rm -rf /", nil); denied {
		t.Error("an empty denylist denies nothing")
	}
}

// A ceiling that fails open is not a ceiling.
func TestDeniedByFailsClosedOnUnparseable(t *testing.T) {
	if _, denied := DeniedBy("git push 'unterminated", []string{"git push"}); !denied {
		t.Error("unparseable input must be refused, not allowed through")
	}
}
