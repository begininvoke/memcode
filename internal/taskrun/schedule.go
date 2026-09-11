package taskrun

import (
	"fmt"
	"time"

	"github.com/memcode-ai/memcode/internal/gateway/occurrence"
	"github.com/memcode-ai/memcode/internal/task"
)

// Deciding what to run after downtime is the whole difficulty of a scheduler
// that lives on a laptop. The machine sleeps, and the question "what did I
// miss?" has three defensible answers, so a task says which one it wants:
//
//	skip      forget it — only a firing that just happened counts
//	run_once  the work still matters, but once, representing the LATEST
//	          missed occurrence (the default, and right for maintenance)
//	catch_up  every missed occurrence matters, up to a hard bound
//
// Every answer is expressed in LOGICAL occurrences, so what runs is decided by
// the calendar rather than by when the daemon woke up.

// Spec converts a task trigger into the timing form the occurrence package
// understands.
func Spec(tr task.Trigger) occurrence.Spec {
	return occurrence.Spec{Cron: tr.Cron, Every: tr.Every, At: tr.At, TZ: tr.TZ}
}

// Decision is what a poll concluded for one trigger.
type Decision struct {
	// Fire are the logical occurrences to run, oldest first. Usually zero or one.
	Fire []time.Time
	// Advance is the new watermark: the newest occurrence now accounted for,
	// whether it ran or was deliberately dropped. Persisting it is what stops a
	// dropped occurrence being rediscovered as missed on the next poll.
	Advance time.Time
	// Dropped counts occurrences deliberately not run. Surfaced on the resulting
	// run so a collapsed backlog is visible rather than silently discarded.
	Dropped int
	// Why explains a non-obvious decision, for the run record.
	Why string
}

// Fresh bounds what counts as "this just happened" rather than "this was
// missed". Generous relative to the poll interval so a slow tick is never
// mistaken for downtime.
const Fresh = 2 * time.Minute

// Due computes what to run for one trigger, given the watermark of the last
// occurrence already accounted for.
//
// A zero `last` means the trigger has never been seen. It is then PLACED at its
// most recent past occurrence rather than treated as having missed everything
// since the epoch — adding a weekly task on a Friday must not immediately fire
// it for every Monday in history.
func Due(tr task.Trigger, last, now time.Time) (Decision, error) {
	if tr.Manual {
		return Decision{}, nil
	}
	spec := Spec(tr)
	if spec.Kind() == "" {
		return Decision{}, fmt.Errorf("trigger has no cadence")
	}

	if last.IsZero() {
		prev, ok, err := occurrence.Prev(spec, now, 0)
		if err != nil {
			return Decision{}, err
		}
		if !ok {
			// Nothing has ever been due (a future one-shot). Watermark just
			// before now so the coming occurrence is still seen.
			return Decision{Advance: now.Add(-time.Nanosecond)}, nil
		}
		// Placed, not fired: the occurrences before a task existed are not its
		// missed work.
		return Decision{Advance: prev}, nil
	}

	// The cap is only about catch_up. The other policies collapse to one run or
	// none regardless of backlog size, so they need a small window, not history.
	limit := 1
	if tr.Missed == task.MissedCatchUp {
		limit = tr.MaxCatchUp
		if limit <= 0 {
			limit = task.DefaultMaxCatchUp
		}
		if limit > task.HardMaxCatchUp {
			limit = task.HardMaxCatchUp
		}
	}

	times, dropped, err := occurrence.Between(spec, last, now, limit)
	if err != nil {
		return Decision{}, err
	}
	if len(times) == 0 {
		return Decision{Advance: last}, nil
	}
	newest := times[len(times)-1]

	switch tr.Missed {
	case task.MissedSkip:
		// Only a firing that JUST happened counts; anything older was missed and
		// this policy forgets it. Note the watermark still advances: a dropped
		// occurrence is accounted for, not rediscovered next poll.
		if now.Sub(newest) <= Fresh {
			return Decision{Fire: []time.Time{newest}, Advance: newest, Dropped: dropped + len(times) - 1}, nil
		}
		return Decision{
			Advance: newest,
			Dropped: dropped + len(times),
			Why:     fmt.Sprintf("missed: skip — %d occurrence(s) passed while nothing was running", dropped+len(times)),
		}, nil

	case task.MissedCatchUp:
		d := Decision{Fire: times, Advance: newest, Dropped: dropped}
		if dropped > 0 {
			d.Why = fmt.Sprintf("catch_up capped at %d — %d older occurrence(s) skipped", limit, dropped)
		}
		return d, nil

	default: // run_once
		// One run, and it REPRESENTS the latest missed occurrence rather than
		// "now". That is what makes the recovered run explainable: the ledger
		// says which Monday it was standing in for.
		d := Decision{Fire: []time.Time{newest}, Advance: newest, Dropped: dropped + len(times) - 1}
		if d.Dropped > 0 {
			d.Why = fmt.Sprintf("missed: run_once — recovering the occurrence of %s, %d older one(s) skipped",
				newest.UTC().Format(time.RFC3339), d.Dropped)
		}
		return d, nil
	}
}

// OccurrenceID is the ledger identity of one firing of one trigger.
func OccurrenceID(tr task.Trigger, at time.Time) string {
	return occurrence.ID(Spec(tr), at)
}

// TriggerKey identifies the SCHEDULE, for storing its watermark.
func TriggerKey(tr task.Trigger) string { return Spec(tr).Key() }
