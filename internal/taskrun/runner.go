package taskrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/jobs"
	"github.com/memcode-ai/memcode/internal/task"
	"github.com/memcode-ai/memcode/internal/taskgit"
)

// Runner turns a task definition into a durable, executed Run.
//
// The order matters and is the point of the whole design:
//
//	freeze inputs -> create the row (claims the occurrence) -> claim the work
//	-> execute -> persist the outcome
//
// The row exists before any work starts, so a duplicate dispatch is rejected by
// the ledger rather than by hoping two executions do not overlap.
type Runner struct {
	Store *Store
	// Spawn runs the agent and blocks until it finishes. Injectable so tests
	// exercise the whole lifecycle — including crashes — without a model call.
	Spawn SpawnFunc
	// HeartbeatEvery bounds how stale a live run's heartbeat can get.
	HeartbeatEvery time.Duration
}

// SpawnFunc executes a task's work and reports what happened.
type SpawnFunc func(ctx context.Context, req SpawnRequest) (SpawnResult, error)

// SpawnRequest is everything the executor needs, taken from the FROZEN run
// rather than from the task file, so a mid-run YAML edit cannot reach it.
type SpawnRequest struct {
	RunID string
	// Project is the repository that owns the run's bookkeeping — its job log
	// outlives a disposable worktree because of this.
	Project string
	// WorkDir is where the work actually happens: the isolated worktree, or the
	// project itself when there is nothing to isolate.
	WorkDir      string
	Instructions string
	Mode         permissions.Mode
	ReadOnly     bool
	// DenyTools and DenyCommands are the capability ceiling, projected from the
	// task's grants. Both are real restrictions on the child.
	DenyTools    []string
	DenyCommands []string
	Timeout      time.Duration
}

// SpawnResult is the executor's report.
type SpawnResult struct {
	Text     string
	LogPath  string
	ExitCode int
}

// NewRunner builds a runner backed by real detached memcode processes.
func NewRunner(store *Store) *Runner {
	return &Runner{Store: store, Spawn: spawnJob, HeartbeatEvery: 20 * time.Second}
}

// Freeze captures a task's execution inputs as a Run, resolving everything that
// could otherwise drift: the definition's revision AND full text, the absolute
// project path, the expanded authority, the runtime, the limits.
//
// Nothing downstream reads the task file again. That is what makes a run mean
// one fixed thing forever, and it is why the definition is stored whole rather
// than by reference — a run stays readable after its file is edited or deleted.
func Freeze(t task.Task, root, triggerKind, triggerID string, now time.Time) (Run, error) {
	return FreezeAt(t, root, triggerKind, triggerID, now, now, 0)
}

// FreezeAt is Freeze with the LOGICAL occurrence this run stands for, and the
// size of the backlog collapsed into it. A run_once recovery of Monday's
// occurrence executed on Wednesday is occurredAt Monday: the ledger then says
// which firing was recovered, instead of only when someone got round to it.
func FreezeAt(t task.Task, root, triggerKind, triggerID string, occurredAt, now time.Time, backlog int) (Run, error) {
	rev, err := t.Revision()
	if err != nil {
		return Run{}, err
	}
	project, err := t.ResolveProject(root)
	if err != nil {
		return Run{}, err
	}
	grants, err := t.Grants()
	if err != nil {
		return Run{}, err
	}
	def, err := task.Marshal(t)
	if err != nil {
		return Run{}, err
	}
	gs := make([]string, len(grants))
	for i, g := range grants {
		gs[i] = string(g)
	}
	return Run{
		ID:          NewID(now),
		Task:        t.Name,
		Revision:    rev,
		Definition:  string(def),
		TriggerKind: triggerKind,
		TriggerID:   triggerID,
		Project:     project,
		Grants:      gs,
		Provider:    t.Agent.Provider,
		Model:       t.Agent.Model,
		Timeout:     t.Timeout(),
		MaxCostUSD:  t.Limits.MaxCostUSD,
		StartedAt:   now,
		OccurredAt:  occurredAt,
		Backlog:     backlog,
		Seen:        SeenUnseen,
	}, nil
}

// modeFor picks the permission mode a run executes under. Both tiers use
// ModeAuto: Safe and Medium run unattended, Dangerous and catastrophic still
// prompt, find nobody, and are therefore refused.
//
// Read-only is deliberately NOT expressed as a stricter mode. ModeAsk looks like
// it should work — no human means every prompt is a denial — but it does not,
// and a real run proved it: a read_only task was asked to create a file and did,
// logging "auto-allowed" in ask mode. The authorization judge downgrades a
// prompt to an allow when the request plainly asked for the action, which is a
// good rule for an interactive session (it catches an agent freelancing PAST
// what the user wanted) and exactly backwards for a task, whose instructions
// always authorize the task's own work.
//
// So authority is enforced as CAPABILITY instead: see readOnlyFor. A tool the
// child was never given cannot be argued for.
func modeFor(task.Task) permissions.Mode { return permissions.ModeAuto }

// Start freezes, records and claims a run without executing it. Returns
// ErrOccupied when the occurrence already belongs to another run.
func (r *Runner) Start(ctx context.Context, t task.Task, root, triggerKind, triggerID string, now time.Time) (Run, error) {
	if !t.IsEnabled() {
		return Run{}, fmt.Errorf("task %q is disabled", t.Name)
	}
	frozen, err := Freeze(t, root, triggerKind, triggerID, now)
	if err != nil {
		return Run{}, err
	}
	return r.Store.Create(ctx, frozen)
}

// Execute claims and runs an already-created run to completion, persisting the
// outcome. Safe to call only once per run; the conditional claim enforces that.
func (r *Runner) Execute(ctx context.Context, run Run, t task.Task) (Run, error) {
	claimed, err := r.Store.Claim(ctx, run.ID, time.Now())
	if err != nil {
		return run, err
	}
	if !claimed {
		// Another process got here first, or this run is already finished.
		return r.Store.Get(ctx, run.ID)
	}

	// Exclusive use of the resource this run mutates. Taken AFTER the claim so a
	// run that loses the claim never touches the lease, and released on every
	// path out — including a panic — so a crash is the only way to leave one
	// behind, which the expiry rule then handles.
	key := LeaseKey(t, run.Project)
	if err := r.Store.Acquire(ctx, key, run.ID, run.Task, time.Now()); err != nil {
		var held ErrLeaseHeld
		if errors.As(err, &held) {
			_ = r.Store.Finish(ctx, run.ID, OutcomeBlocked,
				"another run holds this project",
				fmt.Sprintf("Waiting on run %s (task %s). Mutating tasks in one project run "+
					"one at a time; read-only tasks are free to overlap.", held.Holder.RunID, held.Holder.Task),
				"", time.Now())
			return r.Store.Get(ctx, run.ID)
		}
		return run, err
	}
	defer func() { _ = r.Store.Release(context.WithoutCancel(ctx), key, run.ID) }()

	// Heartbeat for as long as the work runs. Its absence is how Reconcile
	// distinguishes a crashed run from a slow one, and the same beat keeps the
	// lease alive so the two notions of "that process is gone" cannot disagree.
	hbCtx, stopHB := context.WithCancel(ctx)
	defer stopHB()
	go r.heartbeat(hbCtx, run.ID, key)

	runCtx := ctx
	if run.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, run.Timeout)
		defer cancel()
	}

	res := r.execute(runCtx, ctx, run, t)
	if ferr := r.Store.FinishResult(ctx, run.ID, res, time.Now()); ferr != nil {
		return run, ferr
	}
	return r.Store.Get(ctx, run.ID)
}

// execute is the phased body of a run: isolate, work, verify, classify.
//
//	isolate   a fresh worktree, so nothing touches the user's checkout
//	work      the agent, with only the capabilities its grants project
//	verify    the task's own checks, run by US and judged on exit codes
//	classify  the outcome, derived from facts
//
// The phases are separate because their verdicts are separate. The agent can
// complete cleanly and the tests can still fail; the run can be blocked before
// the agent ever starts. Collapsing those into one signal loses exactly the
// information a human needs when something goes wrong.
// The return value is NAMED so the cleanup defer can clear the recorded
// worktree path: removing the directory while still reporting where it was
// would leave every successful run pointing at somewhere that does not exist.
func (r *Runner) execute(runCtx, storeCtx context.Context, run Run, t task.Task) (res Result) {
	res = Result{Outcome: OutcomeFailed, ExecStatus: ExecFailed, VerifyStatus: VerifySkipped}

	cap, err := t.Capability()
	if err != nil {
		res.Summary, res.Detail = "capability projection failed", err.Error()
		res.ExecStatus = ExecBlocked
		res.Outcome = OutcomeBlocked
		return res
	}

	// memcode's own state must not turn up in the user's `git status`. An
	// interactive session does this at launch; an unattended run may be the
	// first thing that ever touches this repo, so it does it too. Idempotent,
	// and a no-op when .memcode does not exist.
	config.EnsureGitignore(run.Project)

	// ISOLATE. Every autonomous run works somewhere that is not the user's
	// checkout: a mutating one needs a branch to build on, and a read-only one
	// still needs its test caches to land off the tree someone is sitting in.
	dir := run.Project
	var wt taskgit.Worktree
	if cap.ProtectProject {
		if !taskgit.IsRepo(storeCtx, run.Project) {
			if cap.Mutating {
				res.Summary = "project is not a git repository"
				res.Detail = "A task that changes code runs in an isolated worktree, which needs git. " +
					"Initialize the repository, or set autonomy.level: read_only."
				res.ExecStatus, res.Outcome = ExecBlocked, OutcomeBlocked
				return res
			}
			// Read-only work in a non-repo: nothing to isolate into, and nothing
			// it is allowed to change anyway.
		} else {
			branch := taskgit.BranchName(t.Git.Branch, t.Name, run.ID, time.Now())
			created, cerr := taskgit.Create(storeCtx, run.Project, branch)
			if cerr != nil {
				res.Summary = "could not isolate the run"
				res.Detail = cerr.Error()
				res.ExecStatus, res.Outcome = ExecBlocked, OutcomeBlocked
				return res
			}
			wt = created
			dir = wt.Path
			res.Worktree, res.Branch, res.BaseRev = wt.Path, wt.Branch, wt.Base
			res.ResultRev = wt.Base
			// Cleanup is decided by the OUTCOME, at the end: a failed run's
			// worktree is the evidence someone is about to ask for, and deleting
			// it to stay tidy destroys it.
			defer func() {
				if keepWorktree(res) {
					return
				}
				_ = taskgit.Remove(storeCtx, wt)
				res.Worktree = ""
			}()
		}
	}

	// WORK.
	spawn, serr := r.Spawn(runCtx, SpawnRequest{
		RunID:        run.ID,
		Project:      run.Project,
		WorkDir:      dir,
		Instructions: t.Instructions,
		Mode:         modeFor(t),
		ReadOnly:     false,
		DenyTools:    cap.DenyTools,
		DenyCommands: cap.DenyCommands,
		Timeout:      run.Timeout,
	})
	res.LogPath = spawn.LogPath
	switch {
	case runCtx.Err() == context.DeadlineExceeded:
		res.ExecStatus = ExecTimedOut
		res.Summary, res.Detail = "timed out", "The run exceeded its limits.timeout and was stopped."
	case serr != nil:
		res.ExecStatus = ExecFailed
		res.Summary, res.Detail = "the run could not complete", serr.Error()
	case spawn.ExitCode != 0:
		res.ExecStatus = ExecFailed
		res.Summary, res.Detail = fmt.Sprintf("exited %d", spawn.ExitCode), clip(spawn.Text, 4000)
	default:
		res.ExecStatus = ExecCompleted
		res.Summary, res.Detail = firstLine(spawn.Text), clip(spawn.Text, 4000)
	}

	// What actually changed is read from the REPOSITORY, not from the report.
	if wt.Path != "" {
		if rev, rerr := wt.Revision(storeCtx); rerr == nil {
			res.ResultRev = rev
		}
		if ch, cerr := wt.Changed(storeCtx); cerr == nil {
			res.Changed = ch
		}
	}

	// VERIFY. Only if the agent got that far; verifying after a timeout tells
	// nobody anything and costs a test suite.
	if res.ExecStatus == ExecCompleted {
		checks, status := Verify(runCtx, dir, t.Verify.Commands)
		res.VerifyStatus = status
		res.Checks = Summarize(checks)
	}

	// CLASSIFY, from facts. The agent's only entry point is raising
	// needs_attention, which can make the verdict more cautious and never less.
	res.Outcome = Decide(res.ExecStatus, res.VerifyStatus, res.Changed, mentionsNeedsAttention(spawn.Text))
	if res.VerifyStatus == VerifyFail {
		res.Summary = "verification failed"
	}

	// PUBLISH. Only a verified change is offered for review: publishing work
	// whose tests failed would put a broken branch in front of someone as if it
	// were ready. A failure keeps its worktree instead, which is where the
	// evidence is.
	if wt.Path != "" && res.OKOutcome() && res.Changed {
		r.publish(storeCtx, run, t, wt, &res)
		if res.PRURL != "" {
			res.Summary = fmt.Sprintf("%s (%s)", res.Summary, res.PRURL)
		}
	}
	if strings.TrimSpace(res.Summary) == "" {
		// An agent that finished without a closing line still needs a legible
		// row in the history and the inbox.
		res.Summary = defaultSummary(res)
	}
	if res.Checks != "" {
		res.Detail = strings.TrimSpace(res.Detail + "\n\n" + res.Checks)
	}
	if res.Worktree != "" && !res.OKOutcome() {
		res.Detail = strings.TrimSpace(res.Detail + "\n\nWorktree kept for inspection: " + res.Worktree)
	}
	return res
}

// defaultSummary describes a run that said nothing about itself, from what it
// actually did.
func defaultSummary(res Result) string {
	switch {
	case res.PRURL != "":
		return "opened " + res.PRURL
	case res.CommitSHA != "" && res.CreatedBranch:
		return fmt.Sprintf("pushed %s to %s", short(res.CommitSHA), res.Branch)
	case res.CommitSHA != "":
		return fmt.Sprintf("committed %s on %s", short(res.CommitSHA), res.Branch)
	case res.Changed:
		return "changed files"
	case res.Outcome == OutcomeNoChange:
		return "nothing to change"
	}
	return string(res.Outcome)
}

// OKOutcome reports whether the result is one nobody needs to look at.
func (r Result) OKOutcome() bool {
	return r.Outcome == OutcomeSuccess || r.Outcome == OutcomeNoChange
}

// Run is the whole loop: freeze, record, claim, execute, persist.// Run is the whole loop: freeze, record, claim, execute, persist.
func (r *Runner) Run(ctx context.Context, t task.Task, root, triggerKind, triggerID string) (Run, error) {
	run, err := r.Start(ctx, t, root, triggerKind, triggerID, time.Now())
	if err != nil {
		return Run{}, err
	}
	return r.Execute(ctx, run, t)
}

func (r *Runner) heartbeat(ctx context.Context, id, leaseKey string) {
	every := r.HeartbeatEvery
	if every <= 0 {
		every = 20 * time.Second
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			// Detached from ctx on purpose: a cancelled run should still record
			// its last heartbeat rather than look stale to Reconcile.
			hbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = r.Store.Heartbeat(hbCtx, id, now)
			_ = r.Store.Renew(hbCtx, leaseKey, id, now)
			cancel()
		}
	}
}

// mentionsNeedsAttention is the agent's ONE input into the verdict: it may raise
// needs_attention, making the outcome more cautious. It has no path to lower
// one — success and no_change are derived from execution status, verification
// exit codes, and whether the repository actually changed.
//
// Reading prose at all is a compromise, kept deliberately narrow. Everything
// that decides whether a run WORKED is now a fact; this only decides whether to
// escalate something that already worked.
func mentionsNeedsAttention(s string) bool {
	l := strings.ToLower(s)
	for _, p := range []string{"needs attention", "needs your", "requires a human",
		"could not decide", "ambiguous",
		// A run refused a capability did not do its job, however calmly it says
		// so. Matched broadly on purpose: this is how a task whose authority is
		// too narrow for its instructions gets noticed instead of reading fine.
		"denied", "blocked", "not permitted", "unable to"} {
		if strings.Contains(l, p) {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return clip(s, 200)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// jobSpec builds the child's spawn spec. Split out and tested directly because
// the real spawn path is the ONE part of a run that an injected fake executor
// cannot cover — and a missing field here is invisible until it matters. It was:
// WorkDir went unset for a while, so the child ran in the user's checkout and
// the agent's file writes escaped the worktree while every test still passed.
func jobSpec(req SpawnRequest, workDir string) jobs.SpawnSpec {
	return jobs.SpawnSpec{
		// Root owns the bookkeeping (job dir, meta, log) so a log outlives a
		// disposable worktree; WorkDir is where the process actually runs, which
		// is what keeps the agent's file writes inside the isolation.
		Root:         req.Project,
		WorkDir:      workDir,
		Task:         req.Instructions,
		Mode:         string(req.Mode),
		RunID:        req.RunID,
		ReadOnly:     req.ReadOnly,
		ToolPolicy:   jobs.ToolPolicy{Disabled: req.DenyTools},
		DenyCommands: req.DenyCommands,
		ReportBack:   true,
	}
}

// spawnJob runs the work as a detached memcode child and waits for it, reusing
// the job machinery the gateway already drives (liveness by pid AND start-time
// signature, a heartbeated meta.json, a durable log).
func spawnJob(ctx context.Context, req SpawnRequest) (SpawnResult, error) {
	workDir := req.WorkDir
	if workDir == "" {
		workDir = req.Project
	}
	if _, err := os.Stat(workDir); err != nil {
		return SpawnResult{}, fmt.Errorf("working directory %s is not reachable: %w", workDir, err)
	}
	job, err := jobs.SpawnWithSpec(jobSpec(req, workDir))
	if err != nil {
		return SpawnResult{}, err
	}
	logPath := jobs.LogPath(req.Project, job.ID)
	for {
		select {
		case <-ctx.Done():
			_ = jobs.Stop(req.Project, job.ID)
			return SpawnResult{LogPath: logPath}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
		cur, err := jobs.Get(req.Project, job.ID)
		if err != nil {
			return SpawnResult{LogPath: logPath}, err
		}
		switch cur.Status {
		case "done":
			return SpawnResult{Text: cur.Result, LogPath: logPath, ExitCode: cur.ExitCode}, nil
		case "failed", "stopped":
			return SpawnResult{Text: cur.Result, LogPath: logPath, ExitCode: cur.ExitCode}, nil
		}
	}
}
