package server

import (
	"context"
	"fmt"
	"io"
	"time"

	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
	"github.com/memcode-ai/memcode/internal/task"
	"github.com/memcode-ai/memcode/internal/taskrun"
)

// Autonomous tasks are polled, not timed.
//
// The gateway's existing schedules install cron timers, which is right for a
// server that stays up. A task ledger has to work on a laptop, where the
// interesting case is the machine being ASLEEP when something was due — and a
// timer that did not fire leaves nothing behind to notice later. Recomputing
// due occurrences from the calendar and a persisted watermark answers "what did
// I miss?" the same way after one missed firing or a hundred, and identically
// after a restart.
//
// This loop deliberately does not reuse the inbox: a task's durable identity is
// its run row, whose unique occurrence index is already the at-least-once
// boundary. Routing through the inbox as well would give one firing two
// competing notions of "already handled".

// taskPollEvery is how often due occurrences are recomputed. Frequent enough
// that a cron minute is not missed by much, cheap enough to ignore: a poll with
// nothing due is a handful of indexed reads.
const taskPollEvery = 30 * time.Second

// taskPollLoop runs due tasks until ctx is cancelled. It re-reads definitions
// every tick, so a task added, edited or disabled while the daemon runs is
// picked up without a restart — the same hot-reload contract schedules have.
func (r *runtime) taskPollLoop(ctx context.Context, out io.Writer) {
	store, err := taskrun.OpenDefault(ctx)
	if err != nil {
		fmt.Fprintf(out, "gateway: task ledger unavailable, autonomous tasks disabled: %v\n", err)
		return
	}
	defer store.Close()

	// Settle anything a previous process left running before deciding what is
	// due, so an interrupted run is never mistaken for one still in flight and
	// its lease can be reclaimed.
	if n, err := store.Reconcile(ctx, time.Now()); err != nil {
		fmt.Fprintf(out, "gateway: reconciling task runs: %v\n", err)
	} else if n > 0 {
		fmt.Fprintf(out, "gateway: %d task run(s) interrupted by an earlier exit\n", n)
	}

	runner := taskrun.NewRunner(store)
	tick := time.NewTicker(taskPollEvery)
	defer tick.Stop()

	// Poll once at startup: the whole point is catching what was missed while
	// this process was not running.
	r.pollTasks(ctx, runner, out)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			r.pollTasks(ctx, runner, out)
		}
	}
}

func (r *runtime) pollTasks(ctx context.Context, runner *taskrun.Runner, out io.Writer) {
	root := r.taskRoot()
	tasks, errs := task.Load(root, time.Now())
	for _, err := range errs {
		fmt.Fprintf(out, "gateway: task definition ignored: %v\n", err)
	}
	if len(tasks) == 0 {
		return
	}
	res := runner.Poll(ctx, tasks, root, time.Now())
	for _, err := range res.Errs {
		fmt.Fprintf(out, "gateway: task: %v\n", err)
	}
	for _, run := range res.Started {
		fmt.Fprintf(out, "gateway: task %s → %s (%s)\n", run.Task, run.Outcome, run.ID)
	}
}

// taskRoot is the project a task without its own `project:` runs in: the
// gateway's default project. A task that names its own project ignores this.
func (r *runtime) taskRoot() string {
	s := r.cfg()
	if s.DefaultProject == "" {
		return ""
	}
	if p, ok := s.Projects[s.DefaultProject]; ok {
		if root, err := gwconfig.CanonicalRoot(p.Path); err == nil {
			return root
		}
	}
	return ""
}
