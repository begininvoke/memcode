package task

import "sort"

// Capability projection: grants decide what a run CAN do, before it starts.
//
// This is the authority boundary. The permission gate and the authorization
// judge still run, and still catch things, but a run's ceiling must never
// depend on either of them reaching the right conclusion — a judge that decides
// the instructions implied consent is exactly how a read-only task talked its
// way into writing a file. A capability the child was never given cannot be
// argued for, so the ceiling is expressed as absence.
//
// The projection has three arms, because no single one covers the ground:
//
//	Tools            what the model can call at all
//	DenyCommands     what a shell command may not be, whatever its risk
//	ProtectProject   whether the user's checkout is even reachable
//
// The second exists because mode cannot express a capability. An unattended run
// needs Medium actions to be useful (a build, a test) and `git push` is also
// Medium, so no mode both runs the tests and withholds the push.

// Capability is the projection of a task's grants onto what its run receives.
type Capability struct {
	// DenyTools are tool names or toolset names the child must not have.
	DenyTools []string
	// DenyCommands are shell command patterns the child may not run, matched
	// against the AST (see permissions.DeniedBy) so wrappers cannot smuggle one.
	DenyCommands []string
	// ProtectProject means the run must not execute in the user's checkout.
	// The runner satisfies it with a throwaway worktree, which is what makes
	// "read-only" mean "cannot change YOUR project" rather than the impractical
	// "performs no writes anywhere" — builds and tests write caches and temp
	// files constantly, and forbidding that would forbid the work.
	ProtectProject bool
	// Mutating reports whether this run may change project state at all. Drives
	// worktree retention and the concurrency lease.
	Mutating bool
}

// alwaysDenied are commands no autonomous run may execute, at any tier.
//
// These are not risk judgements — the risk ladder already refuses most of them
// unattended. They are capability statements: an autonomous run may PREPARE a
// change for a human and may never make one authoritative, so the operations
// that publish, merge, or rewrite shared history are absent from every tier
// rather than merely discouraged in most.
var alwaysDenied = []string{
	"git push --force",
	"git push -f",
	"git push --force-with-lease",
	"git push --mirror",
	"git push --delete",
	"git merge",
	"git rebase",
	"git reset --hard",
	"gh pr merge",
	"gh release create",
	"gh workflow run",
}

// Project returns the capability projection for a task.
func (t Task) Capability() (Capability, error) {
	grants, err := t.Grants()
	if err != nil {
		return Capability{}, err
	}
	has := map[Grant]bool{}
	for _, g := range grants {
		has[g] = true
	}

	c := Capability{
		Mutating:     has[GrantFilesystemMutate],
		DenyCommands: append([]string(nil), alwaysDenied...),
	}
	// Every autonomous run is isolated from the user's checkout. A mutating run
	// needs a branch to put its work on; a read-only run needs somewhere its
	// test caches can land without touching the tree someone is sitting in.
	c.ProtectProject = true

	if !has[GrantFilesystemMutate] {
		// No mutation tools at all, per the tier's contract. Commands still run,
		// because a read-only task that cannot build or test is not useful, and
		// the worktree is what keeps those writes off the user's project.
		c.DenyTools = append(c.DenyTools, "edit_file", "apply_patch")
	}
	if !has[GrantGitCommit] {
		c.DenyCommands = append(c.DenyCommands, "git commit", "git am", "git cherry-pick")
	}
	if !has[GrantGitCreateBranch] {
		c.DenyCommands = append(c.DenyCommands, "git branch", "git checkout -b", "git switch -c")
	}
	if !has[GrantGitPushBranch] {
		// THE gap this mechanism exists for: push is Medium, so an unattended
		// run in auto mode would otherwise do it without ever being asked.
		c.DenyCommands = append(c.DenyCommands, "git push")
	}
	if !has[GrantGitHubOpenPR] {
		// The tool goes, and so does the CLI behind it — a denied tool that can
		// be reached through bash is not a ceiling.
		c.DenyTools = append(c.DenyTools, "github")
		c.DenyCommands = append(c.DenyCommands, "gh", "glab")
	}
	if !has[GrantProcessExec] && !has[GrantProcessExecReadOnly] {
		c.DenyTools = append(c.DenyTools, "shell")
	}

	sort.Strings(c.DenyTools)
	sort.Strings(c.DenyCommands)
	return c, nil
}
