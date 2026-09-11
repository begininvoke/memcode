package taskrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/jobs"
	"github.com/memcode-ai/memcode/internal/task"
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
	RunID        string
	Project      string
	Instructions string
	Mode         permissions.Mode
	ReadOnly     bool
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

// readOnlyFor reports whether the child runs in explorer mode — the read-only
// tool whitelist, no edits and no bash. Reusing the existing mechanism rather
// than hand-listing tools to deny, because a hand-list silently stops covering
// every tool added after it was written.
//
// Note this is currently STRICTER than the read_only tier's stated grants: it
// also removes process.execute_readonly, so a read-only task cannot run `go
// test` today. Honouring that grant needs per-grant command gating, which is
// milestone 4. Too strict is the right direction to be wrong in.
func readOnlyFor(t task.Task) bool { return t.ReadOnly() }

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

	res, err := r.Spawn(runCtx, SpawnRequest{
		RunID:        run.ID,
		Project:      run.Project,
		Instructions: t.Instructions,
		Mode:         modeFor(t),
		ReadOnly:     readOnlyFor(t),
		Timeout:      run.Timeout,
	})
	stopHB()

	outcome, summary, detail := classify(res, err, runCtx)
	if ferr := r.Store.Finish(ctx, run.ID, outcome, summary, detail, res.LogPath, time.Now()); ferr != nil {
		return run, ferr
	}
	return r.Store.Get(ctx, run.ID)
}

// Run is the whole loop: freeze, record, claim, execute, persist.
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

// classify turns an executor result into an outcome. It is deliberately
// conservative: anything it cannot read as a clean success is surfaced for a
// human rather than quietly called done.
func classify(res SpawnResult, err error, ctx context.Context) (Outcome, string, string) {
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return OutcomeFailed, "timed out", "The run exceeded its limits.timeout and was stopped."
	case err != nil:
		return OutcomeFailed, "the run could not complete", err.Error()
	case res.ExitCode != 0:
		return OutcomeFailed, fmt.Sprintf("exited %d", res.ExitCode), clip(res.Text, 4000)
	}
	summary := firstLine(res.Text)
	// The agent saying it changed nothing is a distinct, healthy result, and
	// worth keeping separate from "did work".
	if mentionsNoChange(res.Text) {
		return OutcomeNoChange, summary, clip(res.Text, 4000)
	}
	if mentionsNeedsAttention(res.Text) {
		return OutcomeNeedsAttention, summary, clip(res.Text, 4000)
	}
	return OutcomeSuccess, summary, clip(res.Text, 4000)
}

// These read the agent's own PROSE, which is a provisional heuristic and not a
// reliable classifier — a run that was refused a capability reported it three
// different ways across three attempts ("was denied", "is being denied", "the run was
// blocked"), and chasing phrasings is a losing game. The real fix is a
// structured verdict from the executor plus the verify commands, which is
// milestone 4. Until then these err toward SURFACING: a false needs_attention
// costs a glance, a false success hides a task that silently stopped working.
func mentionsNoChange(s string) bool {
	l := strings.ToLower(s)
	for _, p := range []string{"no changes", "nothing to change", "nothing to do",
		"already up to date", "no updates needed"} {
		if strings.Contains(l, p) {
			return true
		}
	}
	return false
}

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

// spawnJob runs the work as a detached memcode child and waits for it, reusing
// the job machinery the gateway already drives (liveness by pid AND start-time
// signature, a heartbeated meta.json, a durable log).
func spawnJob(ctx context.Context, req SpawnRequest) (SpawnResult, error) {
	if _, err := os.Stat(req.Project); err != nil {
		return SpawnResult{}, fmt.Errorf("project %s is not reachable: %w", req.Project, err)
	}
	job, err := jobs.SpawnWithSpec(jobs.SpawnSpec{
		Root:       req.Project,
		Task:       req.Instructions,
		Mode:       string(req.Mode),
		RunID:      req.RunID,
		ReadOnly:   req.ReadOnly,
		ReportBack: true,
	})
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
