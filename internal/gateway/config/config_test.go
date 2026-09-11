package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDirAndPath(t *testing.T) {
	// XDG_CONFIG_HOME wins and both the gateway config and its operational state
	// resolve under the same global dir (never a repo).
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if dir != "/tmp/xdg/memcode" {
		t.Errorf("Dir() = %q, want /tmp/xdg/memcode", dir)
	}
	p, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "gateway.yaml"); p != want {
		t.Errorf("Path() = %q, want %q (inside Dir)", p, want)
	}
}

func TestResolveProject(t *testing.T) {
	real := t.TempDir()
	// A registered path reached through a symlink must resolve to the real dir —
	// the canonical root is the execution authority, not the config string.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	s := Settings{Projects: map[string]Project{
		"app":     {Path: link, Enabled: true},
		"off":     {Path: real, Enabled: false},
		"missing": {Path: filepath.Join(real, "nope"), Enabled: true},
	}}

	got, err := s.ResolveProject("app")
	if err != nil {
		t.Fatalf("ResolveProject(app): %v", err)
	}
	realResolved, _ := filepath.EvalSymlinks(real)
	if got != realResolved {
		t.Errorf("resolved root = %q, want canonical %q (symlink not resolved)", got, realResolved)
	}

	if _, err := s.ResolveProject("unregistered"); err == nil {
		t.Error("an unregistered id must be refused (no arbitrary root reaches execution)")
	}
	if _, err := s.ResolveProject("off"); err == nil {
		t.Error("a disabled project must be refused")
	}
	if _, err := s.ResolveProject("missing"); err == nil {
		t.Error("a non-existent path must be refused")
	}
}

func TestAllowed(t *testing.T) {
	s := Settings{Channels: map[string]Channel{
		"telegram": {AllowFrom: []string{"@tim", "123"}},
		"discord":  {AllowFrom: []string{"*"}},
		"slack":    {}, // configured but no one allowed
	}}

	cases := []struct {
		channel, principal string
		want               bool
	}{
		{"telegram", "@tim", true},
		{"telegram", "123", true},
		{"telegram", "@eve", false},
		{"discord", "anyone", true}, // wildcard
		{"slack", "@tim", false},    // empty allow-list = deny
		{"unknown", "@tim", false},  // unconfigured channel = deny
	}
	for _, c := range cases {
		if got := s.Allowed(c.channel, c.principal); got != c.want {
			t.Errorf("Allowed(%q,%q) = %v, want %v", c.channel, c.principal, got, c.want)
		}
	}

	// The global escape hatch allows everyone everywhere.
	open := Settings{AllowAll: true}
	if !open.Allowed("telegram", "@anybody") {
		t.Error("AllowAll should permit any principal")
	}
}

func TestAgentAutonomyFieldsAndValidation(t *testing.T) {
	// An ordinary agent stays valid and stays non-autonomous by default —
	// autonomy is never acquired implicitly.
	ordinary := Settings{Agents: map[string]Agent{"ordinary": {Model: "m"}}}
	if err := ordinary.Validate(); err != nil {
		t.Fatalf("ordinary agent must remain valid: %v", err)
	}
	if a := ordinary.Agents["ordinary"]; a.Autonomous || a.Unattended() || a.Objective != "" {
		t.Fatalf("ordinary agent defaulted to autonomy: %+v", a)
	}

	// Objective and Autonomous are independent: holding a goal is not
	// permission to pursue it unprompted.
	goalOnly := Agent{Objective: "find backend roles"}
	if goalOnly.Unattended() {
		t.Fatal("an objective alone must not make an agent unattended")
	}
	// ...and an agent may run unattended with no standing objective (scheduled
	// work under governance), which is the case a single overloaded switch
	// could not express.
	scheduled := Agent{Autonomous: true}
	if !scheduled.Unattended() {
		t.Fatal("autonomous with no objective must still be governed as unattended")
	}

	for _, br := range []string{"", BrowserEphemeral, BrowserExistingChrome} {
		s := Settings{Agents: map[string]Agent{"a": {Browser: br}}}
		if err := s.Validate(); err != nil {
			t.Fatalf("browser %q rejected: %v", br, err)
		}
	}
	bad := Settings{Agents: map[string]Agent{"a": {Browser: "safari"}}}
	if err := bad.Validate(); err == nil {
		t.Fatal("unknown browser backend accepted")
	}
}
func TestGetZeroValue(t *testing.T) {
	var s Settings // nil Channels map
	if got := s.Get("telegram"); !reflect.DeepEqual(got, Channel{}) {
		t.Errorf("Get on nil map = %+v, want zero Channel", got)
	}
}

// Pairing defaults per channel kind: chat channels offer codes to unknown DM
// senders; email never does unless explicitly opted in — the watched mailbox
// may be a personal inbox, and pairing replies would be an auto-responder.
func TestPairingEnabledDefaults(t *testing.T) {
	var s Settings
	if !s.PairingEnabled("telegram") {
		t.Error("telegram pairing should default on")
	}
	if s.PairingEnabled("email") {
		t.Error("email pairing should default OFF")
	}
	on, off := true, false
	s.Channels = map[string]Channel{"email": {Pairing: &on}, "telegram": {Pairing: &off}}
	if !s.PairingEnabled("email") {
		t.Error("explicit email pairing:true ignored")
	}
	if s.PairingEnabled("telegram") {
		t.Error("explicit telegram pairing:false ignored")
	}
}

// A removed setting must fail loudly, not vanish. `kind: personal` used to mean
// "runs on its own"; YAML would silently ignore it now, leaving an agent that
// looks configured but never wakes again.
func TestLegacyKindIsRejectedWithAFix(t *testing.T) {
	s := Settings{Agents: map[string]Agent{"demo": {LegacyKind: "personal"}}}
	err := s.Validate()
	if err == nil {
		t.Fatal("legacy kind silently accepted — an agent would quietly stop running")
	}
	for _, want := range []string{"autonomous: true", "objective:", "demo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should tell the user how to fix it; missing %q in: %v", want, err)
		}
	}
}

// One file holds every subsystem's configuration, but a stale stanza in one
// section must not make unrelated commands impossible. Authorizing a runtime
// has nothing to do with agent definitions.
func TestValidateSectionsIsScoped(t *testing.T) {
	stale := Settings{
		Agents: map[string]Agent{"demo": {LegacyKind: "personal"}},
		AuthorizedRuntimes: []RuntimeGrant{
			{ID: "auth_1", Runtime: "codex", Scope: "all", GrantedAt: "2026-09-12T00:00:00Z"},
		},
	}
	if err := stale.ValidateSections(SectionRuntimeAuth); err != nil {
		t.Errorf("a stale agent must not block runtime-auth validation: %v", err)
	}
	// The agent section itself is still refused, loudly.
	if err := stale.ValidateSections(SectionAgents); err == nil {
		t.Error("the removed kind: field must still be rejected")
	}
	if err := stale.Validate(); err == nil {
		t.Error("whole-file validation must still catch it")
	}
}

// The migration error has to be actionable: this is a KNOWN migration with
// known semantics, so it says exactly what to write.
func TestLegacyKindErrorGivesTheMigration(t *testing.T) {
	s := Settings{Agents: map[string]Agent{"demo": {LegacyKind: "personal"}}}
	err := s.ValidateSections(SectionAgents)
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{
		"kind: personal",   // what they have
		"autonomous: true", // what replaces the permission half
		"objective:",       // what replaces the intent half
		"untouched",        // that their agent home survives
		"demo",             // which agent
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the migration error should mention %q:\n%s", want, msg)
		}
	}
}

// Narrow validation must not become silent data loss: a section this caller
// never validated must round-trip verbatim, legacy fields included.
func TestSaveForPreservesUnvalidatedSections(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	p, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	// A config a whole-file Validate would reject.
	if err := os.WriteFile(p, []byte("agents:\n    demo:\n        kind: personal\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := LoadFor(SectionRuntimeAuth)
	if err != nil {
		t.Fatalf("loading for one section must not trip over another: %v", err)
	}
	s.AuthorizedRuntimes = append(s.AuthorizedRuntimes, RuntimeGrant{
		ID: "auth_x", Runtime: "codex", Scope: "all", GrantedAt: "2026-09-12T00:00:00Z",
	})
	if err := SaveFor(s, SectionRuntimeAuth); err != nil {
		t.Fatalf("SaveFor: %v", err)
	}

	back, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	got := string(back)
	if !strings.Contains(got, "kind: personal") {
		t.Errorf("the untouched agent stanza must survive verbatim — neither dropped nor migrated:\n%s", got)
	}
	if !strings.Contains(got, "auth_x") {
		t.Errorf("the new authorization should be written:\n%s", got)
	}
	// And the file is still rejected by a whole-file load, because nothing here
	// migrated anything on the user's behalf.
	if _, err := Load(); err == nil {
		t.Error("a narrow save must not quietly fix the user's config")
	}
}

func TestRuntimeAuthValidation(t *testing.T) {
	cases := []struct {
		name  string
		grant RuntimeGrant
		bad   bool
	}{
		{"ok all", RuntimeGrant{Runtime: "codex", Scope: "all"}, false},
		{"ok task", RuntimeGrant{Runtime: "codex", Scope: "task", Task: "nightly"}, false},
		{"ok run", RuntimeGrant{Runtime: "codex", Scope: "run", Run: "run_1"}, false},
		{"no runtime", RuntimeGrant{Scope: "all"}, true},
		{"bad scope", RuntimeGrant{Runtime: "codex", Scope: "forever"}, true},
		{"task without name", RuntimeGrant{Runtime: "codex", Scope: "task"}, true},
		{"run without id", RuntimeGrant{Runtime: "codex", Scope: "run"}, true},
		// A runtime this build does not know is INERT, not fatal: config must not
		// depend on the runtime registry.
		{"unknown runtime is not fatal", RuntimeGrant{Runtime: "some-future-thing", Scope: "all"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := Settings{AuthorizedRuntimes: []RuntimeGrant{c.grant}}
			err := s.ValidateSections(SectionRuntimeAuth)
			if c.bad && err == nil {
				t.Error("expected a validation error")
			}
			if !c.bad && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}
