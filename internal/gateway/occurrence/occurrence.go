// Package occurrence computes the LOGICAL fire times of a recurring schedule.
//
// The distinction this package exists to defend: an occurrence is a property of
// the schedule and the calendar, not of when a daemon happened to wake up. A
// weekly task due Monday 10:00 has exactly one Monday-10:00 occurrence whether
// the machine was awake for it, noticed it eight hours late, or was restarted
// three times in between. That is what lets a duplicate scheduler callback, a
// restart, and a late catch-up all resolve to the SAME ledger identity, so the
// unique (task, occurrence) constraint can do its job.
//
// It lives under internal/gateway because that is where cron parsing is allowed
// to live (internal/guard.TestSingleCronParser): one scheduler, one parser.
package occurrence

import (
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// Spec is a trigger's timing, in the same three forms the rest of memcode uses.
// Exactly one of Cron, Every or At is set.
type Spec struct {
	Cron  string
	Every string
	At    string
	TZ    string // named zone for Cron; empty = local
}

// Kind names the spec's form, for identities and error messages.
func (s Spec) Kind() string {
	switch {
	case strings.TrimSpace(s.Cron) != "":
		return "cron"
	case strings.TrimSpace(s.Every) != "":
		return "every"
	case strings.TrimSpace(s.At) != "":
		return "at"
	}
	return ""
}

// Key identifies the SCHEDULE (not one firing of it), so per-trigger state can
// be stored and a cadence edit starts a fresh series rather than inheriting the
// old one's position.
func (s Spec) Key() string {
	switch s.Kind() {
	case "cron":
		return "cron:" + strings.TrimSpace(s.Cron) + "|" + s.TZ
	case "every":
		return "every:" + strings.TrimSpace(s.Every)
	case "at":
		return "at:" + strings.TrimSpace(s.At)
	}
	return ""
}

// ID is the durable identity of ONE logical firing. This is the string that
// becomes the ledger's occurrence key, so it must depend only on the schedule
// and the calendar — never on wall-clock-at-discovery, hostname, or attempt.
func ID(s Spec, at time.Time) string {
	return s.Key() + "@" + at.UTC().Format(time.RFC3339)
}

// parse resolves a spec to something that can enumerate fire times.
func (s Spec) parse() (cron.Schedule, error) {
	switch s.Kind() {
	case "cron":
		expr := strings.TrimSpace(s.Cron)
		if s.TZ != "" {
			if _, err := time.LoadLocation(s.TZ); err != nil {
				return nil, fmt.Errorf("bad tz %q: %w", s.TZ, err)
			}
			expr = "CRON_TZ=" + s.TZ + " " + expr
		}
		sched, err := cron.ParseStandard(expr)
		if err != nil {
			return nil, fmt.Errorf("bad cron %q: %w", s.Cron, err)
		}
		return sched, nil
	case "every":
		d, err := time.ParseDuration(strings.TrimSpace(s.Every))
		if err != nil {
			return nil, fmt.Errorf("bad every %q: %w", s.Every, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("every must be positive, got %q", s.Every)
		}
		return everySchedule{d}, nil
	case "at":
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(s.At))
		if err != nil {
			return nil, fmt.Errorf("bad at %q: %w", s.At, err)
		}
		return atSchedule{t}, nil
	}
	return nil, fmt.Errorf("trigger has no cron, every or at")
}

// everySchedule anchors a fixed interval to the Unix epoch rather than to
// whenever the process started. "every 6h" therefore means 00:00, 06:00, 12:00
// and 18:00 UTC for everybody, forever — the same occurrences before and after
// a restart. Anchoring to process start would make the identity of an
// occurrence depend on daemon uptime, which is exactly what must not happen.
type everySchedule struct{ d time.Duration }

func (e everySchedule) Next(t time.Time) time.Time {
	if e.d <= 0 {
		return time.Time{}
	}
	// Strictly after t, so enumeration always advances.
	n := t.UTC().UnixNano()/int64(e.d) + 1
	return time.Unix(0, n*int64(e.d)).UTC()
}

// prevCapable is implemented by schedules whose previous occurrence is
// computable directly. Scanning forward to find it is fine for a daily cron and
// catastrophic for a 2-minute interval: a year's lookback is 263,000 steps,
// which blows any scan bound and yields a wildly wrong answer rather than a slow
// one. That is not hypothetical — it shipped, placed a new trigger's watermark
// three months in the past, and produced a run claiming a backlog of 100,000.
type prevCapable interface{ prev(time.Time) time.Time }

func (e everySchedule) prev(t time.Time) time.Time {
	if e.d <= 0 {
		return time.Time{}
	}
	n := t.UTC().UnixNano() / int64(e.d)
	return time.Unix(0, n*int64(e.d)).UTC()
}

// atSchedule fires exactly once.
type atSchedule struct{ t time.Time }

func (a atSchedule) prev(t time.Time) time.Time {
	if !a.t.After(t) {
		return a.t.UTC()
	}
	return time.Time{}
}

func (a atSchedule) Next(t time.Time) time.Time {
	if a.t.After(t) {
		return a.t.UTC()
	}
	return time.Time{} // no further occurrences
}

// maxScan bounds enumeration so a tiny interval and a long absence cannot spin.
// Reached only when a machine has been away for an absurd number of periods,
// and the caller is told how many were dropped.
const maxScan = 100_000

// Between returns the logical occurrences in the half-open window (after, until],
// oldest first, at most limit of them.
//
// When there are more than limit, the MOST RECENT are kept and the count of
// dropped older ones is returned. Recency is the right bias: a stale hourly
// occurrence from three weeks ago has almost no value, and replaying thousands
// of them is how a laptop returning from a long sleep turns into a stampede.
func Between(s Spec, after, until time.Time, limit int) (times []time.Time, dropped int, err error) {
	sched, err := s.parse()
	if err != nil {
		return nil, 0, err
	}
	if !until.After(after) {
		return nil, 0, nil
	}
	if limit <= 0 {
		limit = 1
	}

	// Start the scan near the END of the window when the window is enormous.
	// Enumerating from `after` is correct but unbounded: a machine away for
	// months with a short interval would walk millions of occurrences to reach
	// the handful that will actually run.
	start, estimatedDrop := scanStart(sched, after, until, limit)
	dropped += estimatedDrop

	cur := start
	for i := 0; i < maxScan; i++ {
		next := sched.Next(cur)
		if next.IsZero() || next.After(until) {
			break
		}
		times = append(times, next.UTC())
		if len(times) > limit {
			// Keep the tail: the most recent occurrences are the ones worth
			// running, and memory stays bounded however long the absence was.
			times = times[1:]
			dropped++
		}
		cur = next
	}
	return times, dropped, nil
}

// scanStart picks where to begin enumerating, and how many occurrences that
// choice skips. It returns `after` unchanged whenever the whole window is small
// enough to walk honestly, so the dropped count is exact in the common case; it
// is an ESTIMATE only when the window was too large to enumerate, which is
// precisely when nobody needs the exact figure.
func scanStart(sched cron.Schedule, after, until time.Time, limit int) (time.Time, int) {
	interval := estimateInterval(sched, until)
	if interval <= 0 {
		return after, 0
	}
	span := until.Sub(after)
	if span/interval <= time.Duration(maxScan) {
		return after, 0 // walkable; exact counting
	}
	// Enough room for the occurrences we intend to keep, and no more.
	start := until.Add(-time.Duration(limit+2) * interval)
	if !start.After(after) {
		return after, 0
	}
	return start, int(start.Sub(after) / interval)
}

// estimateInterval measures the spacing between two consecutive occurrences near
// t. Exact for a fixed interval, and a good local approximation for a cron whose
// spacing varies (a weekday-only schedule is 1 day four times and 3 days once).
func estimateInterval(sched cron.Schedule, t time.Time) time.Duration {
	a := sched.Next(t)
	if a.IsZero() {
		return 0
	}
	b := sched.Next(a)
	if b.IsZero() {
		return 0
	}
	return b.Sub(a)
}

// Prev returns the most recent logical occurrence at or before t. Used to place
// a NEWLY SEEN trigger in its series: without it a brand-new weekly task would
// look like it had missed every Monday since the epoch.
func Prev(s Spec, t time.Time, lookback time.Duration) (time.Time, bool, error) {
	sched, err := s.parse()
	if err != nil {
		return time.Time{}, false, err
	}
	if lookback <= 0 {
		lookback = 366 * 24 * time.Hour
	}
	// Direct where possible. A fixed interval and a one-shot both know their own
	// previous occurrence; only cron has to be walked.
	if p, ok := sched.(prevCapable); ok {
		got := p.prev(t)
		if got.IsZero() {
			return time.Time{}, false, nil
		}
		return got, true, nil
	}
	// Cron: walk forward from an adaptive start rather than a fixed year, so a
	// frequent schedule does not exceed the scan bound and return an answer from
	// months ago. Doubling finds a recent occurrence in a few passes for any
	// cadence from per-minute to yearly.
	interval := estimateInterval(sched, t)
	if interval <= 0 {
		return time.Time{}, false, nil
	}
	for window := interval * 2; window <= lookback*2; window *= 2 {
		cur := t.Add(-window)
		var last time.Time
		found := false
		for i := 0; i < maxScan; i++ {
			next := sched.Next(cur)
			if next.IsZero() || next.After(t) {
				break
			}
			last, found = next.UTC(), true
			cur = next
		}
		if found {
			return last, true, nil
		}
	}
	return time.Time{}, false, nil
}

// Next returns the next logical occurrence strictly after t, for display.
func Next(s Spec, t time.Time) (time.Time, bool, error) {
	sched, err := s.parse()
	if err != nil {
		return time.Time{}, false, err
	}
	n := sched.Next(t)
	if n.IsZero() {
		return time.Time{}, false, nil
	}
	return n.UTC(), true, nil
}
