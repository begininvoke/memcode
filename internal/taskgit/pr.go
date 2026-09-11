package taskgit

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Pull requests go through the `gh` CLI, which is the only path that can open
// one: memcode's GitHub App has the scopes but no code path, and REPOS.md is
// explicit that the gateway must own authorization before one exists. `gh`
// carries the user's own credential, so an autonomous PR is opened as them —
// which is correct, and worth knowing.

// ErrPRUnsupported means a pull request cannot be opened HERE — gh is missing,
// not signed in, or the remote is not a GitHub host.
//
// This is deliberately distinct from a PR that failed to open. The work is
// already pushed and reachable either way, and a repository that simply is not
// on GitHub should not make every run of every task report a problem. It is
// reported plainly on the run instead.
type ErrPRUnsupported struct{ Reason string }

func (e ErrPRUnsupported) Error() string { return e.Reason }

// ghAvailable reports whether the gh CLI is installed and authenticated.
func ghAvailable(ctx context.Context, dir string) error {
	if _, err := exec.LookPath("gh"); err != nil {
		return ErrPRUnsupported{Reason: "gh is not installed — a task that opens pull requests needs it (https://cli.github.com)"}
	}
	cmd := exec.CommandContext(ctx, "gh", "auth", "status")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return ErrPRUnsupported{Reason: "gh is not authenticated (run `gh auth login`): " + strings.TrimSpace(string(out))}
	}
	return nil
}

func gh(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("gh %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(string(out)), nil
}

// FindPR returns an existing open pull request for a branch.
//
// This is what makes PR creation idempotent. A retry, a restart, or a second
// attempt inside one run must find the pull request the previous attempt opened
// instead of opening a duplicate — duplicates are noise a human then has to
// clean up, and they make the run record ambiguous about which one is real.
func FindPR(ctx context.Context, dir, branch string) (number int, url string, found bool) {
	out, err := gh(ctx, dir, "pr", "list", "--head", branch, "--state", "open",
		"--json", "number,url", "--limit", "1")
	if err != nil || strings.TrimSpace(out) == "" {
		return 0, "", false
	}
	var prs []struct {
		Number int    `json:"number"`
		URL    string `json:"url"`
	}
	if err := json.Unmarshal([]byte(out), &prs); err != nil || len(prs) == 0 {
		return 0, "", false
	}
	return prs[0].Number, prs[0].URL, true
}

// OpenPR creates a pull request, or returns the one already open for the branch.
func OpenPR(ctx context.Context, dir, branch, base, title, body string) (number int, url string, created bool, err error) {
	if n, u, found := FindPR(ctx, dir, branch); found {
		return n, u, false, nil
	}
	if err := ghAvailable(ctx, dir); err != nil {
		return 0, "", false, err
	}
	out, err := gh(ctx, dir, "pr", "create", "--head", branch, "--base", base,
		"--title", title, "--body", body)
	if err != nil {
		if notGitHub(err) {
			return 0, "", false, ErrPRUnsupported{Reason: "this repository's remote is not a GitHub host, so no pull request can be opened"}
		}
		return 0, "", false, err
	}
	url = lastURL(out)
	if url == "" {
		// Created but the URL was not echoed; look it up rather than reporting
		// a PR with no way to reach it.
		if n, u, found := FindPR(ctx, dir, branch); found {
			return n, u, true, nil
		}
		return 0, "", true, nil
	}
	return numberFromURL(url), url, true, nil
}

// notGitHub recognises gh refusing because the remote is not GitHub.
func notGitHub(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "none of the git remotes") ||
		strings.Contains(msg, "not a github") ||
		strings.Contains(msg, "no git remotes found")
}

func lastURL(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "https://") {
			return line
		}
	}
	return ""
}

func numberFromURL(u string) int {
	i := strings.LastIndexByte(u, '/')
	if i < 0 {
		return 0
	}
	n, err := strconv.Atoi(u[i+1:])
	if err != nil {
		return 0
	}
	return n
}

// Provenance is what an autonomous change must say about itself.
type Provenance struct {
	Task         string
	RunID        string
	TaskRevision string
	Base         string
	Occurrence   string
	Verification string
	Description  string
}

// CommitMessage renders a deterministic commit message. Derived from the task,
// never from model output: a commit subject generated per-run drifts in style
// and can carry whatever the model felt like saying, and the one thing this
// message must do is identify what made the change.
func CommitMessage(p Provenance) string {
	subject := p.Description
	if strings.TrimSpace(subject) == "" {
		subject = "changes from autonomous task " + p.Task
	}
	subject = firstLine(subject)
	if len(subject) > 68 {
		subject = strings.TrimSpace(subject[:68]) + "…"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", subject)
	fmt.Fprintf(&b, "Autonomous task: %s\n", p.Task)
	fmt.Fprintf(&b, "Run: %s\n", p.RunID)
	fmt.Fprintf(&b, "Task revision: %s\n", p.TaskRevision)
	fmt.Fprintf(&b, "Base: %s\n", p.Base)
	return b.String()
}

// PRTitle renders the pull request title.
func PRTitle(p Provenance) string {
	t := p.Description
	if strings.TrimSpace(t) == "" {
		t = "Autonomous task: " + p.Task
	}
	return firstLine(t)
}

// PRBody renders the pull request body.
//
// Provenance is carried automatically so an autonomous change is auditable from
// GitHub alone — what made it, which revision of the task, what it was based on,
// and what was actually verified. Someone reviewing this should not have to
// open memcode's local database to find out where it came from.
func PRBody(p Provenance) string {
	var b strings.Builder
	b.WriteString("Opened by an autonomous memcode task. No human wrote this change.\n\n")
	fmt.Fprintf(&b, "- **Task:** `%s`\n", p.Task)
	fmt.Fprintf(&b, "- **Run:** `%s`\n", p.RunID)
	fmt.Fprintf(&b, "- **Task revision:** `%s`\n", p.TaskRevision)
	fmt.Fprintf(&b, "- **Base:** `%s`\n", p.Base)
	if p.Occurrence != "" {
		fmt.Fprintf(&b, "- **Occurrence:** `%s`\n", p.Occurrence)
	}
	b.WriteString("\n### Verification\n\n")
	if strings.TrimSpace(p.Verification) == "" {
		b.WriteString("The task declared no verification commands, so nothing was checked automatically.\n")
	} else {
		fmt.Fprintf(&b, "```\n%s\n```\n", strings.TrimSpace(p.Verification))
	}
	b.WriteString("\nThe task's authority stops here: it may prepare a change for review and " +
		"cannot merge, force-push, or deploy.\n")
	return b.String()
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
