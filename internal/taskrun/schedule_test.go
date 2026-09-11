package taskrun

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/task"
)

func at(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func triggered(t *testing.T, body string) task.Task {
	t.Helper()
	tk, err := task.Parse([]byte(body), "test.yaml", task.ScopeProject, at(t, "2026-09-11T12:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

const daily = `version: 1
name: nightly
instructions: do the nightly thing
triggers:
  - cron: "0 2 * * *"
    tz: UTC
    missed: %s
`

// A brand-new trigger is PLACED in its series, not treated as having missed
// every occurrence since the epoch.
func TestNewTriggerDoesNotFireForHistory(t *testing.T) {
	tk := triggered(t, fmt.Sprintf(daily, "run_once"))
	now := at(t, "2026-09-11T12:00:00Z")

	d, err := Due(tk.Triggers[0], time.Time{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Fire) != 0 {
		t.Errorf("a new trigger must not fire on sight, got %v", d.Fire)
	}
	if got := d.Advance.Format(time.RFC3339); got != "2026-09-11T02:00:00Z" {
		t.Errorf("watermark = %s, want this morning's occurrence", got)
	}
}

// INVARIANT: run_once after downtime runs ONCE, and the run represents a
// specific missed logical occurrence rather than "now".
func TestRunOnceRecoversTheLatestOccurrence(t *testing.T) {
	tk := triggered(t, fmt.Sprintf(daily, "run_once"))
	last := at(t, "2026-09-08T02:00:00Z") // last fired Tuesday
	now := at(t, "2026-09-11T09:30:00Z")  // laptop reopened Friday morning

	d, err := Due(tk.Triggers[0], last, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Fire) != 1 {
		t.Fatalf("run_once must fire exactly once, got %d", len(d.Fire))
	}
	// Three occurrences passed (9th, 10th, 11th); the newest is the one recovered.
	if got := d.Fire[0].Format(time.RFC3339); got != "2026-09-11T02:00:00Z" {
		t.Errorf("recovered occurrence = %s, want the most recent missed one", got)
	}
	if d.Dropped != 2 {
		t.Errorf("dropped = %d, want the 2 older missed occurrences", d.Dropped)
	}
	if !strings.Contains(d.Why, "2026-09-11T02:00:00Z") {
		t.Errorf("why = %q, must name the occurrence being recovered", d.Why)
	}
	// And the watermark advances past all of them, so the next poll is quiet.
	next, _ := Due(tk.Triggers[0], d.Advance, now)
	if len(next.Fire) != 0 {
		t.Errorf("a second poll must not re-fire, got %v", next.Fire)
	}
}

// skip forgets a missed occurrence, but still accounts for it — otherwise it is
// rediscovered as missed on every future poll.
func TestSkipDropsMissedButAdvances(t *testing.T) {
	tk := triggered(t, fmt.Sprintf(daily, "skip"))
	last := at(t, "2026-09-08T02:00:00Z")
	now := at(t, "2026-09-11T09:30:00Z")

	d, err := Due(tk.Triggers[0], last, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Fire) != 0 {
		t.Errorf("skip must not run missed work, got %v", d.Fire)
	}
	if d.Advance.IsZero() || d.Advance.Before(last) {
		t.Error("skip must still advance the watermark")
	}
	if d.Dropped == 0 {
		t.Error("skip must report what it dropped")
	}

	// A firing that JUST happened is not "missed" and does run.
	fresh := at(t, "2026-09-11T02:00:30Z")
	d2, _ := Due(tk.Triggers[0], at(t, "2026-09-10T02:00:00Z"), fresh)
	if len(d2.Fire) != 1 {
		t.Errorf("a just-happened occurrence must still run under skip, got %v", d2.Fire)
	}
}

// INVARIANT: catch_up is bounded. A laptop back after months must not enqueue
// thousands of runs.
func TestCatchUpIsCappedAndReportsBacklog(t *testing.T) {
	tk := triggered(t, `version: 1
name: hourly
instructions: x
triggers:
  - every: 1h
    missed: catch_up
    max_catch_up: 5
`)
	last := at(t, "2026-01-01T00:00:00Z")
	now := at(t, "2026-09-11T00:00:00Z") // ~6100 hours later

	d, err := Due(tk.Triggers[0], last, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Fire) != 5 {
		t.Fatalf("fired %d, want the cap of 5", len(d.Fire))
	}
	if d.Dropped < 6000 {
		t.Errorf("dropped = %d, want the thousands it collapsed", d.Dropped)
	}
	if !strings.Contains(d.Why, "skipped") {
		t.Errorf("why = %q, must surface the collapsed backlog", d.Why)
	}
	// The ones kept are the most recent, not the oldest.
	if now.Sub(d.Fire[0]) > 6*time.Hour {
		t.Errorf("kept window starts %s before now — should be the newest", now.Sub(d.Fire[0]))
	}
}

// A YAML value is a ceiling REQUEST, not an override.
func TestCatchUpHardLimitRejectedAtParse(t *testing.T) {
	_, err := task.Parse([]byte(`version: 1
name: greedy
instructions: x
triggers:
  - every: 1h
    missed: catch_up
    max_catch_up: 5000
`), "test.yaml", task.ScopeProject, time.Now())
	if err == nil || !strings.Contains(err.Error(), "hard limit") {
		t.Errorf("err = %v, want the hard catch-up limit enforced", err)
	}
}

func TestCatchUpDefaultsWhenUnset(t *testing.T) {
	tk := triggered(t, `version: 1
name: hourly
instructions: x
triggers:
  - every: 1h
    missed: catch_up
`)
	if got := tk.Triggers[0].MaxCatchUp; got != task.DefaultMaxCatchUp {
		t.Errorf("max_catch_up = %d, want the default %d", got, task.DefaultMaxCatchUp)
	}
}

// INVARIANT: the same logical occurrence dispatched twice yields ONE run, even
// across "processes". This is the restart story at the ledger level.
func TestPollIsIdempotentAcrossRestart(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	root := t.TempDir()
	tk := triggered(t, fmt.Sprintf(daily, "run_once"))
	now := at(t, "2026-09-11T09:30:00Z")

	var spawns int
	var mu sync.Mutex
	r := runner(t, s, func(context.Context, SpawnRequest) (SpawnResult, error) {
		mu.Lock()
		spawns++
		mu.Unlock()
		return SpawnResult{Text: "done"}, nil
	})
	// Place the watermark as if it last ran on Tuesday.
	if err := s.SetWatermark(ctx, tk.Name, TriggerKey(tk.Triggers[0]), at(t, "2026-09-08T02:00:00Z"), now); err != nil {
		t.Fatal(err)
	}

	first := r.Poll(ctx, []task.Task{tk}, root, now)
	if len(first.Errs) != 0 {
		t.Fatalf("errs: %v", first.Errs)
	}
	if len(first.Started) != 1 {
		t.Fatalf("started %d runs, want 1", len(first.Started))
	}

	// A "restart": rewind the watermark as if the advance had not been durably
	// written, then poll again. The occurrence index must still hold the line.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE trigger_state SET last_occurrence=? WHERE task=?`,
		ts(at(t, "2026-09-08T02:00:00Z")), tk.Name); err != nil {
		t.Fatal(err)
	}
	second := r.Poll(ctx, []task.Task{tk}, root, now)
	if len(second.Started) != 0 {
		t.Errorf("a rediscovered occurrence must not run again, started %d", len(second.Started))
	}
	if spawns != 1 {
		t.Errorf("executed %d times, want exactly 1 for one occurrence", spawns)
	}

	runs, _ := s.Recent(ctx, tk.Name, 10)
	if len(runs) != 1 {
		t.Fatalf("ledger has %d runs for one occurrence, want 1", len(runs))
	}
	// The run explains which occurrence it stands for.
	if got := runs[0].OccurredAt.Format(time.RFC3339); got != "2026-09-11T02:00:00Z" {
		t.Errorf("occurred_at = %s, want the recovered occurrence", got)
	}
	if runs[0].Backlog != 2 {
		t.Errorf("backlog = %d, want the 2 collapsed occurrences", runs[0].Backlog)
	}
	if !strings.Contains(runs[0].Detail, "run_once") {
		t.Errorf("detail = %q, must explain the recovery", runs[0].Detail)
	}
}

// INVARIANT: mutating runs in one project serialize; read-only runs overlap.
func TestLeaseSerializesMutatingRunsOnly(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	now := time.Now()

	mutating := sample(t, "")
	readonly := sample(t, "version: 1\nname: audit\ninstructions: look\nautonomy:\n  level: read_only\n")

	if k := LeaseKey(mutating, "/repo"); k != "project:/repo" {
		t.Errorf("mutating lease key = %q, want the project", k)
	}
	if k := LeaseKey(readonly, "/repo"); k != "" {
		t.Errorf("read-only lease key = %q, want none — read-only runs may overlap", k)
	}

	// One holder at a time.
	if err := s.Acquire(ctx, "project:/repo", "run_a", "a", now); err != nil {
		t.Fatal(err)
	}
	err := s.Acquire(ctx, "project:/repo", "run_b", "b", now)
	var held ErrLeaseHeld
	if !errorsAs(err, &held) {
		t.Fatalf("second acquire = %v, want ErrLeaseHeld", err)
	}
	if held.Holder.RunID != "run_a" {
		t.Errorf("holder = %q, want run_a", held.Holder.RunID)
	}
	// Re-acquiring our own is idempotent.
	if err := s.Acquire(ctx, "project:/repo", "run_a", "a", now); err != nil {
		t.Errorf("re-acquiring our own lease must succeed, got %v", err)
	}
	// Released, the next run gets it.
	if err := s.Release(ctx, "project:/repo", "run_a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Acquire(ctx, "project:/repo", "run_b", "b", now); err != nil {
		t.Errorf("after release, acquire = %v", err)
	}
	// A read-only run takes no lease and is never blocked.
	if err := s.Acquire(ctx, "", "run_c", "c", now); err != nil {
		t.Errorf("read-only acquire = %v, want a no-op success", err)
	}
}

// INVARIANT: a lease survives process failure safely — the next process can
// reclaim it, by the same liveness rule runs use.
func TestLeaseIsReclaimedFromADeadHolder(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	now := time.Now()

	if err := s.Acquire(ctx, "project:/repo", "run_dead", "a", now); err != nil {
		t.Fatal(err)
	}
	// The holder's process is gone.
	if _, err := s.db.ExecContext(ctx, `UPDATE leases SET pid=? WHERE key=?`, deadPID, "project:/repo"); err != nil {
		t.Fatal(err)
	}
	if err := s.Acquire(ctx, "project:/repo", "run_next", "b", now); err != nil {
		t.Errorf("a dead holder's lease must be reclaimable, got %v", err)
	}
	l, ok, _ := s.lease(ctx, "project:/repo")
	if !ok || l.RunID != "run_next" {
		t.Errorf("lease holder = %+v, want run_next", l)
	}
	// The evicted run must not be able to delete its successor's claim.
	if err := s.Release(ctx, "project:/repo", "run_dead"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.lease(ctx, "project:/repo"); !ok {
		t.Error("an evicted run released a lease it no longer held")
	}
}

// A stale heartbeat expires a lease held on another machine, where we cannot
// ask about a pid.
func TestLeaseExpiresOnStaleHeartbeat(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.Acquire(ctx, "project:/repo", "run_remote", "a", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE leases SET host='elsewhere', pid=1, heartbeat_at=? WHERE key=?`,
		ts(now.Add(-StaleAfter-time.Minute)), "project:/repo"); err != nil {
		t.Fatal(err)
	}
	if err := s.Acquire(ctx, "project:/repo", "run_new", "b", now); err != nil {
		t.Errorf("a stale remote lease must be reclaimable, got %v", err)
	}
}

// A blocked run is recorded, not silently dropped: the ledger must show that a
// task was due and could not proceed.
func TestBlockedRunIsRecorded(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	root := t.TempDir()
	tk := sample(t, "")

	r := runner(t, s, ok("should not run"))
	frozen, err := Freeze(tk, root, TriggerManual, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.Create(ctx, frozen)
	if err != nil {
		t.Fatal(err)
	}
	// Someone else holds the project.
	if err := s.Acquire(ctx, "project:"+run.Project, "other_run", "other", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := r.Execute(ctx, run, tk)
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != OutcomeBlocked {
		t.Errorf("outcome = %q, want blocked", got.Outcome)
	}
	if !strings.Contains(got.Detail, "other_run") {
		t.Errorf("detail = %q, must name what it is waiting on", got.Detail)
	}
}

// The whole milestone, in one test: daemon offline over several occurrences,
// comes back, computes what it missed, runs exactly one durable run for the
// right occurrence, respects the lease, and records the result in the inbox.
func TestDowntimeRecoveryEndToEnd(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	root := t.TempDir()
	tk := triggered(t, fmt.Sprintf(daily, "run_once"))

	r := runner(t, s, ok("nightly work done"))
	// Monday: the daemon is up and the task has run.
	monday := at(t, "2026-09-07T02:00:30Z")
	r.Poll(ctx, []task.Task{tk}, root, monday)

	// ... machine asleep Tuesday through Thursday ...
	friday := at(t, "2026-09-11T09:00:00Z")
	res := r.Poll(ctx, []task.Task{tk}, root, friday)

	if len(res.Errs) != 0 {
		t.Fatalf("errs: %v", res.Errs)
	}
	if len(res.Started) != 1 {
		t.Fatalf("recovery started %d runs, want exactly 1", len(res.Started))
	}
	run := res.Started[0]
	if got := run.OccurredAt.Format(time.RFC3339); got != "2026-09-11T02:00:00Z" {
		t.Errorf("recovered occurrence = %s, want Friday's", got)
	}
	if run.Outcome != OutcomeSuccess {
		t.Errorf("outcome = %q, want success", run.Outcome)
	}
	if run.Backlog == 0 {
		t.Error("the collapsed occurrences must be visible on the run")
	}
	// It is in the inbox, unseen.
	unseen, _ := s.Unseen(ctx, 10)
	found := false
	for _, u := range unseen {
		if u.ID == run.ID {
			found = true
		}
	}
	if !found {
		t.Error("a recovered run must land in the inbox")
	}
	// And the lease it took is gone.
	if _, held, _ := s.lease(ctx, "project:"+run.Project); held {
		t.Error("the lease must be released when the run finishes")
	}
	// Polling again changes nothing.
	again := r.Poll(ctx, []task.Task{tk}, root, friday)
	if len(again.Started) != 0 {
		t.Errorf("a second poll started %d runs, want 0", len(again.Started))
	}
}

// A manual-only task is never fired by a poll.
func TestPollIgnoresManualAndDisabled(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	manual := sample(t, "")
	off := triggered(t, `version: 1
name: paused
instructions: x
enabled: false
triggers:
  - every: 1h
`)
	r := runner(t, s, ok("no"))
	res := r.Poll(ctx, []task.Task{manual, off}, t.TempDir(), time.Now())
	if len(res.Started) != 0 {
		t.Errorf("poll started %d runs, want 0", len(res.Started))
	}
}

func errorsAs(err error, target *ErrLeaseHeld) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(ErrLeaseHeld); ok {
		*target = e
		return true
	}
	return false
}
