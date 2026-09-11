package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepo builds a throwaway repo with a .memcode/tasks file and the inner
// .gitignore memcode writes, so these tests exercise REAL git semantics rather
// than a model of them. The remedy this command prints was wrong on first
// write precisely because it was reasoned about instead of run.
func gitRepo(t *testing.T, rootIgnore string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	for _, d := range []string{".memcode/tasks", ".memcode/jobs/j1"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".memcode/tasks/upgrade.yaml", "version: 1\n")
	write(".memcode/state.db", "binary\n")
	write(".memcode/jobs/j1/log", "log\n")
	// The file EnsureGitignore writes.
	write(".memcode/.gitignore", "# memcode's local state — not part of your repo\n*\n"+
		"# ...except autonomous tasks, which are meant to be reviewed and committed\n!tasks/\n!tasks/**\n")
	if rootIgnore != "" {
		write(".gitignore", rootIgnore)
	}
	return root
}

func ignored(t *testing.T, root, rel string) bool {
	t.Helper()
	err := exec.Command("git", "-C", root, "check-ignore", "-q", rel).Run()
	return err == nil
}

// With no repo-wide rule, memcode's own .memcode/.gitignore is enough: tasks
// are committable and the rest of .memcode stays invisible.
func TestTasksVisibleWithoutRootRule(t *testing.T) {
	root := gitRepo(t, "")
	if ignored(t, root, ".memcode/tasks/upgrade.yaml") {
		t.Error("tasks must be visible to git with no root rule")
	}
	for _, rel := range []string{".memcode/state.db", ".memcode/jobs/j1/log"} {
		if !ignored(t, root, rel) {
			t.Errorf("%s must stay ignored — only tasks/ is carved out", rel)
		}
	}
	if v := checkTaskVisibility(root); v.Shadowed {
		t.Errorf("no warning expected, got %+v", v)
	}
}

// A repo-wide `.memcode/` wins, and the shadowing is detected.
func TestRootRuleShadowsTasks(t *testing.T) {
	root := gitRepo(t, ".memcode/\n")
	if !ignored(t, root, ".memcode/tasks/upgrade.yaml") {
		t.Fatal("precondition: .memcode/ should hide tasks")
	}
	v := checkTaskVisibility(root)
	if !v.Shadowed {
		t.Fatal("shadowing must be detected")
	}
	if !strings.Contains(v.Rule, ".memcode/") {
		t.Errorf("rule = %q, want the offending pattern named", v.Rule)
	}
}

// The point of the whole exercise: APPLYING the printed remedy must actually
// work. Adding an exception under `.memcode/` does not, because git never
// descends into an ignored directory — so the remedy has to REPLACE the rule.
func TestTaskVisibilityRemedy(t *testing.T) {
	root := gitRepo(t, ".memcode/\n")

	// The naive advice — appending an exception — leaves tasks ignored. This is
	// asserted so the wrong fix can never quietly come back.
	if err := os.WriteFile(filepath.Join(root, ".gitignore"),
		[]byte(".memcode/\n!.memcode/tasks/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !ignored(t, root, ".memcode/tasks/upgrade.yaml") {
		t.Error("appending an exception under an ignored parent should NOT work — " +
			"if git changed this, simplify the remedy")
	}

	// The remedy we actually print, applied as a replacement.
	remedy := strings.Join(RemedyLines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(remedy), 0o644); err != nil {
		t.Fatal(err)
	}
	if ignored(t, root, ".memcode/tasks/upgrade.yaml") {
		t.Errorf("the printed remedy must expose tasks, but they are still ignored:\n%s", remedy)
	}
	for _, rel := range []string{".memcode/state.db", ".memcode/jobs/j1/log"} {
		if !ignored(t, root, rel) {
			t.Errorf("the remedy must keep %s ignored — it exposes tasks, not all of .memcode", rel)
		}
	}
	if v := checkTaskVisibility(root); v.Shadowed {
		t.Errorf("after the remedy there should be no warning, got %+v", v)
	}

	// And it must hold even without memcode's inner .gitignore, so the advice
	// is self-sufficient rather than quietly depending on another file.
	if err := os.Remove(filepath.Join(root, ".memcode", ".gitignore")); err != nil {
		t.Fatal(err)
	}
	if ignored(t, root, ".memcode/tasks/upgrade.yaml") {
		t.Error("the remedy must expose tasks on its own, without .memcode/.gitignore")
	}
	if !ignored(t, root, ".memcode/state.db") {
		t.Error("the remedy alone must still ignore the rest of .memcode")
	}
}

// git add must stage the task and nothing else.
func TestRemedyStagesOnlyTasks(t *testing.T) {
	root := gitRepo(t, strings.Join(RemedyLines, "\n")+"\n")
	out, err := exec.Command("git", "-C", root, "add", "-An", ".memcode").Output()
	if err != nil {
		t.Fatalf("git add -An: %v", err)
	}
	got := strings.TrimSpace(string(out))
	if !strings.Contains(got, ".memcode/tasks/upgrade.yaml") {
		t.Errorf("the task should be stageable, got:\n%s", got)
	}
	for _, unwanted := range []string{"state.db", "jobs/"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("%s must not be stageable, got:\n%s", unwanted, got)
		}
	}
}

// A repo with no tasks directory has nothing to warn about.
func TestNoWarningWithoutTasks(t *testing.T) {
	root := t.TempDir()
	if v := checkTaskVisibility(root); v.Shadowed {
		t.Errorf("expected no warning, got %+v", v)
	}
}
