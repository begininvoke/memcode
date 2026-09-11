package taskgit

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Publishing an autonomous change is entirely deterministic. The model decided
// WHAT the diff should be; from here nothing it wrote reaches a git argument.
// Stage, commit, push, open the pull request — each a real check against real
// repository state, because unattended git driven by generated text fails
// silently and expensively: a wrong branch, a clobbered ref, someone else's
// work overwritten.
//
// Every step is idempotent. A retry inside one run must find what the previous
// attempt already did rather than doing it twice, so each step asks the
// repository what is already true before acting.

// DefaultRemote is where an autonomous branch is published.
const DefaultRemote = "origin"

// Publication is what a run left behind, and who created it. The "created"
// flags matter for idempotency: reusing something is not the same as making it,
// and a retry must be able to tell.
type Publication struct {
	Remote        string
	Branch        string
	Commit        string
	PRNumber      int
	PRURL         string
	CreatedBranch bool
	CreatedCommit bool
	CreatedPR     bool
}

// stagePathspec stages everything the run intended and nothing it did not.
//
// `git add -A` alone is too blunt: it would sweep up memcode's own local state
// if a run happened to create any. Gitignore is still honoured, so generated
// files a repo already ignores stay out; this is the belt to that's braces.
var stagePathspec = []string{".", ":(exclude).memcode", ":(exclude).git"}

// Stage adds the run's work to the index. Returns whether anything was staged.
func Stage(ctx context.Context, w Worktree) (bool, error) {
	args := append([]string{"add", "-A", "--"}, stagePathspec...)
	if _, err := git(ctx, w.Path, args...); err != nil {
		return false, err
	}
	out, err := git(ctx, w.Path, "diff", "--cached", "--name-only")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// Commit records the staged work and returns the new commit.
//
// Idempotent: a worktree already advanced past its base with nothing left
// staged has been committed by a previous attempt, and that commit is returned
// rather than a second empty one being made.
func Commit(ctx context.Context, w Worktree, message string) (sha string, created bool, err error) {
	staged, err := Stage(ctx, w)
	if err != nil {
		return "", false, err
	}
	if !staged {
		head, herr := HeadRevision(ctx, w.Path)
		if herr != nil {
			return "", false, herr
		}
		if head != w.Base {
			return head, false, nil // a previous attempt already committed this
		}
		// Nothing staged and nothing committed: there is no change. An empty
		// commit is an artifact manufactured to satisfy configuration, and the
		// caller is expected to have short-circuited before reaching here.
		return "", false, fmt.Errorf("nothing to commit")
	}
	if _, err := git(ctx, w.Path, "-c", "user.name=memcode", "-c", "user.email=tasks@memcode.ai",
		"commit", "--no-verify", "-m", message); err != nil {
		return "", false, err
	}
	head, err := HeadRevision(ctx, w.Path)
	if err != nil {
		return "", false, err
	}
	return head, true, nil
}

// DefaultBranch resolves the remote's default branch, falling back through the
// conventional names. Used to refuse publishing onto it and as the PR base.
func DefaultBranch(ctx context.Context, repo, remote string) string {
	if out, err := git(ctx, repo, "symbolic-ref", "--short", "refs/remotes/"+remote+"/HEAD"); err == nil {
		if _, name, ok := strings.Cut(out, "/"); ok && name != "" {
			return name
		}
	}
	for _, candidate := range []string{"main", "master"} {
		if _, err := git(ctx, repo, "rev-parse", "--verify", "refs/remotes/"+remote+"/"+candidate); err == nil {
			return candidate
		}
	}
	return "main"
}

// RemoteExists reports whether the repository has a remote by this name. A repo
// with no remote is a normal local repo, not a broken one: a task there commits
// its work and stops, rather than failing every run over a setup fact.
func RemoteExists(ctx context.Context, repo, remote string) bool {
	out, err := git(ctx, repo, "remote")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == remote {
			return true
		}
	}
	return false
}

// RemoteBranchExists reports whether the remote already has this branch.
func RemoteBranchExists(ctx context.Context, repo, remote, branch string) (bool, error) {
	out, err := git(ctx, repo, "ls-remote", "--heads", remote, "refs/heads/"+branch)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// remoteBranchSHA returns the remote branch's tip, "" when it does not exist.
func remoteBranchSHA(ctx context.Context, repo, remote, branch string) (string, error) {
	out, err := git(ctx, repo, "ls-remote", "--heads", remote, "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(out)
	if line == "" {
		return "", nil
	}
	return strings.Fields(line)[0], nil
}

// ErrBranchNotOurs means the remote branch exists and this run did not create
// it. Refusing is the whole point: an autonomous push must never land on a
// branch a human is using, and a name collision is indistinguishable from one.
type ErrBranchNotOurs struct{ Branch, Remote string }

func (e ErrBranchNotOurs) Error() string {
	return fmt.Sprintf("%s/%s already exists and was not created by this run — refusing to update a branch that is not ours",
		e.Remote, e.Branch)
}

// Push publishes the run's branch. NEW BRANCHES ONLY.
//
// Three refusals, in order of how bad they would be:
//   - never the default branch, or the conventional names for it
//   - never a remote branch this run did not create
//   - never a force update of anything
//
// An existing remote branch whose tip is already our commit is a completed
// previous attempt, and succeeds without doing anything.
func Push(ctx context.Context, w Worktree, remote, commit string, ownedByRun bool) (created bool, err error) {
	if remote == "" {
		remote = DefaultRemote
	}
	def := DefaultBranch(ctx, w.Repo, remote)
	if w.Branch == def || w.Branch == "main" || w.Branch == "master" || w.Branch == "HEAD" {
		return false, fmt.Errorf("refusing to push %q: an autonomous run never publishes to the default branch", w.Branch)
	}

	existing, err := remoteBranchSHA(ctx, w.Repo, remote, w.Branch)
	if err != nil {
		return false, err
	}
	switch {
	case existing == "":
		// Fresh branch: the only case that actually pushes new work.
	case existing == commit:
		return false, nil // a previous attempt already pushed exactly this
	case ownedByRun:
		// Ours, and moved on (a second commit in the same run). Fast-forward
		// only — never --force, at any tier.
	default:
		return false, ErrBranchNotOurs{Branch: w.Branch, Remote: remote}
	}

	// Pushed from the REPOSITORY, not the linked worktree. A relative remote URL
	// ("../origin.git") resolves against the working directory, so pushing from
	// .memcode/worktrees/<run>/ looks for the remote three levels below where it
	// actually is and fails with a bare "could not read from remote repository".
	// The branch lives in the shared object store either way.
	//
	// No --force, no --force-with-lease, no refspec tricks. A rejected push is
	// a signal to stop, not to try harder.
	if _, err := git(ctx, w.Repo, "push", "--set-upstream", remote, w.Branch); err != nil {
		return false, err
	}
	return existing == "", nil
}

// MergeConflicts reports whether merging head into the remote's default branch
// would conflict — i.e. whether the base moved under the run in a way that
// makes the change unmergeable.
//
// It NEVER rebases or rewrites anything. Rewriting history under an unattended
// run is how work disappears; the honest response to drift is to say so and let
// a human decide. ok=false means the check could not be made (an old git), and
// the caller treats that as "no conflict detected" rather than blocking.
func MergeConflicts(ctx context.Context, repo, remote, defaultBranch, head string) (conflicts bool, ok bool) {
	target := remote + "/" + defaultBranch
	if _, err := git(ctx, repo, "rev-parse", "--verify", "refs/remotes/"+target); err != nil {
		return false, false
	}
	// merge-tree --write-tree performs the merge in memory and exits non-zero on
	// conflict. Available since git 2.38; older gits report ok=false.
	cmd := exec.CommandContext(ctx, "git", "merge-tree", "--write-tree", target, head)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err == nil {
		return false, true
	}
	if strings.Contains(string(out), "usage:") || strings.Contains(string(out), "unknown option") {
		return false, false // this git cannot answer
	}
	return true, true
}

// BaseMoved reports whether the remote default branch has advanced past the
// commit this run was based on. Informational: drift alone is normal and fine,
// and only a CONFLICT is a problem.
func BaseMoved(ctx context.Context, repo, remote, defaultBranch, base string) bool {
	target := "refs/remotes/" + remote + "/" + defaultBranch
	if _, err := git(ctx, repo, "rev-parse", "--verify", target); err != nil {
		return false
	}
	// base is an ancestor of the remote tip and not equal to it => it moved on.
	tip, err := git(ctx, repo, "rev-parse", target)
	if err != nil || tip == base {
		return false
	}
	_, err = git(ctx, repo, "merge-base", "--is-ancestor", base, tip)
	return err == nil
}

// Fetch updates remote-tracking refs so drift and conflict checks see the
// current state of the remote rather than a stale local view.
func Fetch(ctx context.Context, repo, remote string) error {
	_, err := git(ctx, repo, "fetch", "--quiet", remote)
	return err
}
