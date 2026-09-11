package task

import (
	"fmt"
	"sort"
	"strings"
)

// Authority in a task file is expressed as GRANTS. A Level is a friendly preset
// that expands to a set of them; the policy engine only ever reasons about the
// expanded set. That split matters because a single tier name is a bad unit of
// authority — "shared state" would have to mean Slack, DNS, deploys, secrets
// and package publishing all at once, which is why this file ships two tiers
// and a named capability list instead.
//
// The allowlist below IS the schema-level floor: a grant that does not appear
// here is refused at parse time. There is deliberately no grant spelling for
// force-push, merge, deploy, or destroying shared state — those are not
// "missing features" to be added by a user editing YAML, they are absent by
// construction. The runtime floor (permissions.Decide returning NeedPrompt for
// anything catastrophic, in every mode) still sits underneath all of this.

// Grant is one named capability an autonomous run may exercise.
type Grant string

const (
	// Read-only capabilities.
	GrantFilesystemRead      Grant = "filesystem.read"
	GrantProcessExecReadOnly Grant = "process.execute_readonly"

	// Local mutation.
	GrantFilesystemMutate Grant = "filesystem.local_mutation"
	GrantProcessExec      Grant = "process.execute"

	// Preparing a change for humans.
	GrantGitCreateBranch Grant = "git.create_branch"
	GrantGitCommit       Grant = "git.commit"
	GrantGitPushBranch   Grant = "git.push_branch"
	GrantGitHubOpenPR    Grant = "github.open_pr"
)

// Level is a preset bundle of grants.
type Level string

const (
	// LevelReadOnly inspects, runs read-only commands, and reports. It changes
	// nothing anywhere, including in a worktree.
	LevelReadOnly Level = "read_only"

	// LevelBranch is the default. It may edit an isolated worktree, run
	// commands, commit, push a NEW branch, and open a pull request — the full
	// shape of "prepare a change for review" and nothing beyond it.
	//
	// Opening a PR belongs here rather than in some higher tier. It does mutate
	// remote state, but a PR is a review artifact: it is precisely the act of
	// asking a human to decide. The invariant this whole feature is built
	// around is that autonomous work may PREPARE a change, never make it
	// authoritative.
	LevelBranch Level = "branch"
)

var levelGrants = map[Level][]Grant{
	LevelReadOnly: {
		GrantFilesystemRead,
		GrantProcessExecReadOnly,
	},
	LevelBranch: {
		GrantFilesystemRead,
		GrantProcessExecReadOnly,
		GrantFilesystemMutate,
		GrantProcessExec,
		GrantGitCreateBranch,
		GrantGitCommit,
		GrantGitPushBranch,
		GrantGitHubOpenPR,
	},
}

// knownGrants is the complete allowlist. Anything outside it is a parse error.
var knownGrants = map[Grant]bool{
	GrantFilesystemRead:      true,
	GrantProcessExecReadOnly: true,
	GrantFilesystemMutate:    true,
	GrantProcessExec:         true,
	GrantGitCreateBranch:     true,
	GrantGitCommit:           true,
	GrantGitPushBranch:       true,
	GrantGitHubOpenPR:        true,
}

// Levels lists the valid levels, for error messages and shell completion.
func Levels() []Level { return []Level{LevelReadOnly, LevelBranch} }

// KnownGrants lists every grant a task file may name, sorted.
func KnownGrants() []Grant {
	out := make([]Grant, 0, len(knownGrants))
	for g := range knownGrants {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ValidLevel reports whether l names a shipped tier.
func ValidLevel(l Level) bool { _, ok := levelGrants[l]; return ok }

// ExpandGrants resolves a task's authority to the concrete set the policy
// engine enforces: the level's preset plus any explicitly named additions,
// deduplicated and sorted. An unknown grant or level is an error — a typo must
// never silently widen or narrow authority.
func ExpandGrants(level Level, extra []Grant) ([]Grant, error) {
	preset, ok := levelGrants[level]
	if !ok {
		return nil, fmt.Errorf("unknown autonomy level %q (use %s)", level, joinLevels(Levels()))
	}
	set := map[Grant]bool{}
	for _, g := range preset {
		set[g] = true
	}
	for _, g := range extra {
		g = Grant(strings.TrimSpace(string(g)))
		if g == "" {
			continue
		}
		if !knownGrants[g] {
			return nil, fmt.Errorf("unknown grant %q (known: %s)", g, joinGrants(KnownGrants()))
		}
		set[g] = true
	}
	out := make([]Grant, 0, len(set))
	for g := range set {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// Grants resolves this task's effective authority.
func (t Task) Grants() ([]Grant, error) {
	return ExpandGrants(t.Autonomy.Level, t.Autonomy.Grants)
}

// HasGrant reports whether the task's expanded authority includes g. A
// resolution error means "no": authority questions fail closed.
func (t Task) HasGrant(g Grant) bool {
	all, err := t.Grants()
	if err != nil {
		return false
	}
	for _, have := range all {
		if have == g {
			return true
		}
	}
	return false
}

// MayMutate reports whether the task can change anything on disk.
func (t Task) MayMutate() bool { return t.HasGrant(GrantFilesystemMutate) }

// MayOpenPR reports whether the task can push a branch AND open a pull request.
// Both are required: a PR with nothing pushed behind it is not a thing.
func (t Task) MayOpenPR() bool {
	return t.HasGrant(GrantGitPushBranch) && t.HasGrant(GrantGitHubOpenPR)
}

func joinLevels(ls []Level) string {
	s := make([]string, len(ls))
	for i, l := range ls {
		s[i] = string(l)
	}
	return strings.Join(s, ", ")
}

func joinGrants(gs []Grant) string {
	s := make([]string, len(gs))
	for i, g := range gs {
		s[i] = string(g)
	}
	return strings.Join(s, ", ")
}
