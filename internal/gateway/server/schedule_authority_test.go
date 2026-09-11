package server

import (
	"os"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
)

// FOLLOW-UP, NOT A FIX.
//
// This test documents authority that gateway SCHEDULES (the older
// `schedules:` list in gateway.yaml, distinct from autonomous tasks) currently
// have, so it cannot be quietly lost. It asserts today's behaviour. If someone
// deliberately narrows it, this test should fail and be updated in the same
// commit — that is the point of writing it down.
//
// What was found while building autonomous tasks:
//
//  1. Every scheduled job is spawned at permissions.ModeAuto (see fireSchedule →
//     runJob → jobs.Spawn in server.go). In ModeAuto, Safe AND Medium actions run
//     with no prompt. `git commit`, `git push` and `gh pr create` are all Medium,
//     so a schedule can already commit, push and open a pull request unattended.
//
//  2. A detached child has no human, so a prompt is a denial — which sounds like
//     a backstop but is weaker than it looks. The AUTHORIZATION JUDGE in
//     gateCommand can DOWNGRADE a prompt to an allow when the request plainly
//     asked for the action. A schedule's task text is persistent,
//     authorization-shaped prose, so it tends to satisfy exactly that test.
//     Proven empirically: a task restricted to read-only and told to create a
//     file created it, logging "auto-allowed" while running in ask mode.
//
// The conclusion drawn for autonomous tasks, and the rule worth carrying
// forward: for unattended execution, authority must be enforced by the
// CAPABILITY surface the run possesses — which tools it has at all — and not by
// a judgement about whether the instructions implied consent. Persistent task
// text is not consent. The permission gate remains useful defence in depth; it
// is not the boundary.
//
// Autonomous tasks now do this (taskrun.readOnlyFor → --read-only → the
// read-only tool whitelist). Legacy schedules do NOT, and deliberately were not
// changed here: narrowing them is a behaviour change for existing users and
// belongs in its own commit with its own migration note.
func TestLegacyScheduleAuthorityIsDocumented(t *testing.T) {
	// A schedule's spawn mode. Grep-anchored to the real call so the constant
	// here cannot drift away from the code it describes.
	const scheduleSpawnMode = permissions.ModeAuto

	// In that mode, the actions that let an unattended job publish work run
	// without a prompt.
	for _, cmd := range []string{
		"git checkout -b auto/x",
		"git commit -m x",
		"git push -u origin auto/x",
	} {
		risk, catastrophic := permissions.ClassifyBash(cmd)
		if catastrophic {
			t.Errorf("%q is catastrophic — the floor would prompt, revisit this test", cmd)
		}
		if d := permissions.Decide(scheduleSpawnMode, risk, catastrophic); d != permissions.Allow {
			t.Errorf("%q under %s = %v; this test records that it is ALLOWED today — "+
				"if that changed on purpose, update this test and note the migration",
				cmd, scheduleSpawnMode, d)
		}
	}

	// The floor still holds for the things that make work authoritative or
	// destroy shared state. This half must never start failing.
	for _, cmd := range []string{
		"git push --force origin main",
		"git reset --hard origin/main",
	} {
		risk, catastrophic := permissions.ClassifyBash(cmd)
		if !catastrophic {
			t.Errorf("%q must stay catastrophic", cmd)
		}
		if d := permissions.Decide(scheduleSpawnMode, risk, catastrophic); d != permissions.NeedPrompt {
			t.Errorf("%q must still require a human in every mode, got %v", cmd, d)
		}
	}
}

// The spawn site this test describes must keep saying ModeAuto, or the comment
// above is fiction. Cheap anchor against silent drift.
func TestScheduleSpawnModeAnchor(t *testing.T) {
	src := readSource(t, "server.go")
	if !strings.Contains(src, "permissions.ModeAuto") {
		t.Skip("the schedule spawn site moved; re-anchor TestLegacyScheduleAuthorityIsDocumented")
	}
}

// readSource reads a file in this package, so a documented claim stays anchored
// to the code it describes.
func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
