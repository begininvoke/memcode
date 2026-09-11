package occurrence

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// An occurrence identity depends only on the schedule and the calendar. Two
// processes discovering the same firing at different wall-clock moments — a
// duplicate callback, a restart, a late catch-up — must produce the same id, or
// the ledger's unique constraint protects nothing.
func TestOccurrenceIDIsDiscoveryIndependent(t *testing.T) {
	s := Spec{Cron: "0 10 * * MON", TZ: "UTC"}
	fire := mustParse(t, "2026-09-14T10:00:00Z")

	onTime := ID(s, fire)
	late := ID(s, fire) // discovered eight hours later; same logical occurrence
	if onTime != late {
		t.Errorf("the same occurrence must have one id: %q vs %q", onTime, late)
	}
	if next := ID(s, fire.Add(7*24*time.Hour)); next == onTime {
		t.Error("a different week must be a different occurrence")
	}
	// The id carries the logical time, not a discovery time.
	if got := ID(s, fire); got != "cron:0 10 * * MON|UTC@2026-09-14T10:00:00Z" {
		t.Errorf("id = %q", got)
	}
}

// Editing the cadence starts a fresh series rather than inheriting the old
// one's position.
func TestKeyDistinguishesCadences(t *testing.T) {
	a := Spec{Cron: "0 10 * * MON", TZ: "UTC"}
	b := Spec{Cron: "0 11 * * MON", TZ: "UTC"}
	c := Spec{Cron: "0 10 * * MON", TZ: "America/Los_Angeles"}
	if a.Key() == b.Key() || a.Key() == c.Key() {
		t.Error("expression and zone must both be part of the schedule key")
	}
}

// "every" anchors to the Unix epoch, not to process start, so the same
// occurrences exist before and after a restart.
func TestEveryIsAnchoredToEpochNotProcessStart(t *testing.T) {
	s := Spec{Every: "6h"}
	// A window that spans a restart: whoever asks, the boundaries are the same.
	from := mustParse(t, "2026-09-11T01:00:00Z")
	to := mustParse(t, "2026-09-11T19:00:00Z")
	got, dropped, err := Between(s, from, to, 10)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0", dropped)
	}
	want := []string{"2026-09-11T06:00:00Z", "2026-09-11T12:00:00Z", "2026-09-11T18:00:00Z"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i].Format(time.RFC3339) != w {
			t.Errorf("occurrence %d = %s, want %s", i, got[i].Format(time.RFC3339), w)
		}
	}
}

func TestBetweenIsHalfOpen(t *testing.T) {
	s := Spec{Every: "1h"}
	at := mustParse(t, "2026-09-11T12:00:00Z")
	// (at, at+1h] includes 13:00 but NOT 12:00 — an occurrence already recorded
	// must never be re-enumerated, or every poll would re-fire the last one.
	got, _, err := Between(s, at, at.Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Format(time.RFC3339) != "2026-09-11T13:00:00Z" {
		t.Errorf("got %v, want just 13:00", got)
	}
}

// A laptop away for months with an hourly task must not enqueue thousands of
// runs. The cap keeps the MOST RECENT and reports how many were dropped.
func TestCatchUpIsBounded(t *testing.T) {
	s := Spec{Every: "1h"}
	from := mustParse(t, "2026-01-01T00:00:00Z")
	to := mustParse(t, "2026-09-11T00:00:00Z") // ~6100 hours
	got, dropped, err := Between(s, from, to, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 {
		t.Fatalf("kept %d occurrences, want the cap of 10", len(got))
	}
	if dropped < 6000 {
		t.Errorf("dropped = %d, want the several thousand it skipped", dropped)
	}
	// Recency bias: the ones kept are the ones adjacent to now.
	last := got[len(got)-1]
	if !last.Equal(to) && to.Sub(last) > time.Hour {
		t.Errorf("kept tail ends at %s, want occurrences adjacent to %s", last, to)
	}
	if first := got[0]; to.Sub(first) > 11*time.Hour {
		t.Errorf("kept window starts at %s — should be the newest 10, not the oldest", first)
	}
}

// A new trigger must be placed in its series, or it looks like it has missed
// every occurrence since the epoch.
func TestPrevPlacesANewTrigger(t *testing.T) {
	s := Spec{Cron: "0 10 * * MON", TZ: "UTC"}
	now := mustParse(t, "2026-09-11T12:00:00Z") // a Friday
	prev, ok, err := Prev(s, now, 30*24*time.Hour)
	if err != nil || !ok {
		t.Fatalf("Prev = %v, %v", ok, err)
	}
	if got := prev.Format(time.RFC3339); got != "2026-09-07T10:00:00Z" {
		t.Errorf("prev = %s, want the Monday just gone", got)
	}
	// Placed there, nothing is due yet.
	due, _, _ := Between(s, prev, now, 10)
	if len(due) != 0 {
		t.Errorf("a freshly placed trigger has nothing due, got %v", due)
	}
}

// A one-shot has exactly one occurrence and never repeats.
func TestAtFiresOnce(t *testing.T) {
	when := "2026-09-12T09:00:00Z"
	s := Spec{At: when}
	before := mustParse(t, "2026-09-11T00:00:00Z")
	after := mustParse(t, "2026-09-13T00:00:00Z")

	got, _, err := Between(s, before, after, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Format(time.RFC3339) != when {
		t.Fatalf("got %v, want one occurrence at %s", got, when)
	}
	// Already past: nothing more, ever.
	again, _, _ := Between(s, got[0], after, 10)
	if len(again) != 0 {
		t.Errorf("a one-shot must not recur, got %v", again)
	}
}

func TestTimeZoneIsHonoured(t *testing.T) {
	// 10:00 in Ho Chi Minh (UTC+7) is 03:00 UTC.
	s := Spec{Cron: "0 10 * * *", TZ: "Asia/Ho_Chi_Minh"}
	from := mustParse(t, "2026-09-11T00:00:00Z")
	to := mustParse(t, "2026-09-11T12:00:00Z")
	got, _, err := Between(s, from, to, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Format(time.RFC3339) != "2026-09-11T03:00:00Z" {
		t.Fatalf("got %v, want 03:00Z", got)
	}
}

func TestBadSpecs(t *testing.T) {
	for _, c := range []Spec{
		{Cron: "not a cron"},
		{Every: "forever"},
		{Every: "-1h"},
		{At: "whenever"},
		{Cron: "0 10 * * *", TZ: "Mars/Olympus"},
		{},
	} {
		if _, _, err := Between(c, time.Now(), time.Now().Add(time.Hour), 5); err == nil {
			t.Errorf("%+v should not parse", c)
		}
	}
}

func TestNextForDisplay(t *testing.T) {
	s := Spec{Cron: "0 10 * * MON", TZ: "UTC"}
	now := mustParse(t, "2026-09-11T12:00:00Z")
	n, ok, err := Next(s, now)
	if err != nil || !ok {
		t.Fatalf("Next = %v, %v", ok, err)
	}
	if got := n.Format(time.RFC3339); got != "2026-09-14T10:00:00Z" {
		t.Errorf("next = %s, want the coming Monday", got)
	}
}

// REGRESSION. Prev used to scan forward from a fixed one-year lookback. For a
// 2-minute interval that is 263,000 steps, which exceeded the scan bound and
// returned an occurrence from three months earlier — so a brand-new trigger was
// placed in the distant past and its first poll claimed a backlog of 100,000.
// Short intervals must be exact and cheap.
func TestPrevIsExactForShortIntervals(t *testing.T) {
	now := mustParse(t, "2026-09-11T16:37:41Z")
	for _, c := range []struct{ every, want string }{
		{"2m", "2026-09-11T16:36:00Z"},
		{"1m", "2026-09-11T16:37:00Z"},
		{"30s", "2026-09-11T16:37:30Z"},
		{"1h", "2026-09-11T16:00:00Z"},
		{"6h", "2026-09-11T12:00:00Z"},
	} {
		got, ok, err := Prev(Spec{Every: c.every}, now, 0)
		if err != nil || !ok {
			t.Errorf("every %s: Prev = %v, %v", c.every, ok, err)
			continue
		}
		if got.Format(time.RFC3339) != c.want {
			t.Errorf("every %s: prev = %s, want %s", c.every, got.Format(time.RFC3339), c.want)
		}
		// And placing a new trigger there leaves nothing due.
		due, dropped, _ := Between(Spec{Every: c.every}, got, now, 10)
		if len(due) != 0 || dropped != 0 {
			t.Errorf("every %s: a freshly placed trigger has %d due / %d dropped, want 0/0",
				c.every, len(due), dropped)
		}
	}
}

// A frequent cron must not blow the scan bound either.
func TestPrevHandlesFrequentCron(t *testing.T) {
	now := mustParse(t, "2026-09-11T16:37:41Z")
	got, ok, err := Prev(Spec{Cron: "*/5 * * * *", TZ: "UTC"}, now, 0)
	if err != nil || !ok {
		t.Fatalf("Prev = %v, %v", ok, err)
	}
	if want := "2026-09-11T16:35:00Z"; got.Format(time.RFC3339) != want {
		t.Errorf("prev = %s, want %s", got.Format(time.RFC3339), want)
	}
}

// A huge window with a short interval must stay cheap AND return the occurrences
// adjacent to `until`, not whatever the scan reached before giving up.
func TestBetweenStaysNearTheEndOfAHugeWindow(t *testing.T) {
	s := Spec{Every: "1m"}
	after := mustParse(t, "2020-01-01T00:00:00Z") // ~3.5 million minutes
	until := mustParse(t, "2026-09-11T16:37:00Z")

	got, dropped, err := Between(s, after, until, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("kept %d, want 3", len(got))
	}
	if last := got[len(got)-1]; !last.Equal(until) {
		t.Errorf("newest kept = %s, want %s — the window must end at `until`",
			last.Format(time.RFC3339), until.Format(time.RFC3339))
	}
	if dropped < 3_000_000 {
		t.Errorf("dropped = %d, want it to reflect the millions skipped", dropped)
	}
}
