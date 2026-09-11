package runtimes

import (
	"strings"
	"testing"
	"time"
)

var clock = time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)

func grant(t *testing.T, runtime string, scope Scope, taskName, runID string) Grant {
	t.Helper()
	g, err := NewGrant(runtime, scope, taskName, runID, clock)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// THE rule this package exists for. A credential sitting on the machine is not
// permission to spend it unattended: the user signed into that other tool, not
// into a scheduler.
func TestAvailabilityIsNotAuthorization(t *testing.T) {
	env := Env{
		Available: []string{"claude-code", "codex", Hosted}, // all present
		Task:      "nightly",
	}
	// Nothing authorized.
	if env.Authorizations.Authorized("claude-code", "nightly", "") {
		t.Error("a present credential must not be authorized by its mere presence")
	}
	// A policy that would happily use it still cannot.
	res, err := Resolve(Policy{
		Strategy: StrategyPreferAuthorized,
		Allowed:  []string{"claude-code", "codex"},
		Model:    ModelAuto,
	}, env)
	if err == nil {
		t.Fatalf("resolved to %q with nothing authorized", res.Runtime)
	}
	var none ErrNoRuntime
	if !asNoRuntime(err, &none) {
		t.Fatalf("err = %T, want ErrNoRuntime", err)
	}
	// And it names what could be authorized, because that is the actionable part.
	if len(none.Offer) == 0 {
		t.Error("a blocked resolution should name the runtimes worth authorizing")
	}
}

// The hosted gateway needs no grant: it is memcode's own metered service, and
// the user's account is the permission.
func TestHostedIsAlwaysAuthorized(t *testing.T) {
	var none Authorizations
	if !none.Authorized(Hosted, "any", "") {
		t.Error("the hosted gateway must not require an authorization")
	}
	res, err := Resolve(Policy{Strategy: StrategyPreferAuthorized,
		Allowed: []string{"claude-code", Hosted}, Model: ModelAuto},
		Env{Available: []string{"claude-code"}, Task: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Runtime != Hosted {
		t.Errorf("runtime = %q, want the hosted fallback when nothing else is authorized", res.Runtime)
	}
	if res.AuthID != "" {
		t.Error("the hosted gateway relies on no grant")
	}
}

// Authorization scope: run, task, all.
func TestAuthorizationScopes(t *testing.T) {
	auth := Authorizations{
		grant(t, "claude-code", ScopeTask, "nightly", ""),
		grant(t, "codex", ScopeAll, "", ""),
	}
	if !auth.Authorized("claude-code", "nightly", "") {
		t.Error("a task-scoped grant must cover its own task")
	}
	if auth.Authorized("claude-code", "other-task", "") {
		t.Error("a task-scoped grant must NOT cover a different task")
	}
	if !auth.Authorized("codex", "anything", "") {
		t.Error("an all-scoped grant covers every task")
	}

	runScoped := Authorizations{grant(t, "grok", ScopeRun, "", "run_1")}
	if !runScoped.Authorized("grok", "t", "run_1") {
		t.Error("a run-scoped grant must cover its own run")
	}
	if runScoped.Authorized("grok", "t", "run_2") {
		t.Error("a run-scoped grant must not leak into another run")
	}
	if runScoped.Authorized("grok", "t", "") {
		t.Error("a run-scoped grant must not become standing permission")
	}
}

// The narrowest applicable grant is the one recorded, because it is the one
// someone would withdraw.
func TestFindPrefersTheNarrowestGrant(t *testing.T) {
	broad := grant(t, "codex", ScopeAll, "", "")
	narrow := grant(t, "codex", ScopeTask, "nightly", "")
	for _, order := range []Authorizations{{broad, narrow}, {narrow, broad}} {
		g, ok := order.Find("codex", "nightly", "")
		if !ok || g.Scope != ScopeTask {
			t.Errorf("found %+v, want the task-scoped grant whatever the order", g)
		}
	}
}

func TestRevokeStopsAuthorizing(t *testing.T) {
	g := grant(t, "codex", ScopeAll, "", "")
	auth := Authorizations{g}
	if !auth.Authorized("codex", "t", "") {
		t.Fatal("precondition")
	}
	revoked, n := auth.Revoke(g.ID)
	if n != 1 {
		t.Fatalf("revoked %d grants, want 1", n)
	}
	if revoked.Authorized("codex", "t", "") {
		t.Error("a revoked grant must stop authorizing")
	}
	// The record survives, so "when did this stop" stays answerable.
	if len(revoked) != 1 || !revoked[0].Revoked {
		t.Error("revocation must keep the grant on the record, marked")
	}
}

func TestNewGrantValidates(t *testing.T) {
	if _, err := NewGrant("nope", ScopeAll, "", "", clock); err == nil {
		t.Error("an unknown runtime must be refused")
	}
	if _, err := NewGrant("codex", "forever", "", "", clock); err == nil {
		t.Error("an unknown scope must be refused")
	}
	if _, err := NewGrant("codex", ScopeTask, "", "", clock); err == nil {
		t.Error("a task-scoped grant needs a task name")
	}
	if _, err := NewGrant("codex", ScopeRun, "", "", clock); err == nil {
		t.Error("a run-scoped grant needs a run id")
	}
}

// SELECTION is deterministic and obeys the user's stated order. It is not a
// cost optimizer: the same inputs must give the same answer every time.
func TestSelectionFollowsUserOrder(t *testing.T) {
	env := Env{
		Available:      []string{"claude-code", "codex", "grok"},
		Authorizations: Authorizations{grant(t, "claude-code", ScopeAll, "", ""), grant(t, "codex", ScopeAll, "", "")},
		Task:           "t",
	}
	first, err := Resolve(Policy{Strategy: StrategyPreferAuthorized,
		Allowed: []string{"codex", "claude-code"}, Model: ModelAuto}, env)
	if err != nil {
		t.Fatal(err)
	}
	if first.Runtime != "codex" {
		t.Errorf("runtime = %q, want the first authorized entry in the user's order", first.Runtime)
	}
	// Reverse the preference; the answer reverses with it.
	second, err := Resolve(Policy{Strategy: StrategyPreferAuthorized,
		Allowed: []string{"claude-code", "codex"}, Model: ModelAuto}, env)
	if err != nil {
		t.Fatal(err)
	}
	if second.Runtime != "claude-code" {
		t.Errorf("runtime = %q, want order to be obeyed rather than optimized", second.Runtime)
	}
	// Deterministic across repeats.
	for i := 0; i < 5; i++ {
		again, _ := Resolve(Policy{Strategy: StrategyPreferAuthorized,
			Allowed: []string{"codex", "claude-code"}, Model: ModelAuto}, env)
		if again.Runtime != first.Runtime {
			t.Fatal("resolution must not vary between identical calls")
		}
	}
}

// An unauthorized entry is skipped, not preferred.
func TestSelectionSkipsUnauthorizedAndUnavailable(t *testing.T) {
	env := Env{
		Available:      []string{"codex"}, // claude-code is NOT installed
		Authorizations: Authorizations{grant(t, "claude-code", ScopeAll, "", ""), grant(t, "codex", ScopeAll, "", "")},
		Task:           "t",
	}
	res, err := Resolve(Policy{Strategy: StrategyPreferAuthorized,
		Allowed: []string{"claude-code", "codex"}, Model: ModelAuto}, env)
	if err != nil {
		t.Fatal(err)
	}
	if res.Runtime != "codex" {
		t.Errorf("runtime = %q — an authorized but absent runtime must be skipped", res.Runtime)
	}
}

// CAPABILITY: a pinned model must be servable by whatever runs it.
func TestCapabilityGatesAPinnedModel(t *testing.T) {
	env := Env{
		Available:      []string{"claude-code", "codex"},
		Authorizations: Authorizations{grant(t, "claude-code", ScopeAll, "", ""), grant(t, "codex", ScopeAll, "", "")},
		Task:           "t",
	}
	// An Anthropic model cannot be served by the Codex subscription.
	res, err := Resolve(Policy{Strategy: StrategyPreferAuthorized,
		Allowed: []string{"codex", "claude-code"}, Model: "claude-sonnet-5"}, env)
	if err != nil {
		t.Fatal(err)
	}
	if res.Runtime != "claude-code" {
		t.Errorf("runtime = %q, want the one that can serve the pinned model", res.Runtime)
	}
	if res.Model != "claude-sonnet-5" {
		t.Errorf("model = %q, want the pin honoured", res.Model)
	}
	// An unknown model only goes somewhere that can refuse it intelligently.
	hosted, ok := Get(Hosted)
	if !ok {
		t.Fatal("hosted runtime missing")
	}
	if !hosted.CanServe(Need{Model: "some-model-nobody-added"}) {
		t.Error("the gateway should accept an unknown id and answer for it")
	}
	cc, _ := Get("claude-code")
	if cc.CanServe(Need{Model: "some-model-nobody-added"}) {
		t.Error("a subscription runtime must not be handed a model it cannot identify")
	}
}

// EXPLICIT never substitutes: that is the entire point of pinning a runtime.
func TestExplicitStrategyNeverSubstitutes(t *testing.T) {
	env := Env{
		Available:      []string{"claude-code", "codex"},
		Authorizations: Authorizations{grant(t, "codex", ScopeAll, "", "")},
		Task:           "t",
	}
	_, err := Resolve(Policy{Strategy: StrategyExplicit,
		Allowed: []string{"claude-code"}, Model: ModelAuto, Fallback: []string{Hosted}}, env)
	if err == nil {
		t.Fatal("an unauthorized explicit runtime must fail rather than run elsewhere")
	}
	if !strings.Contains(err.Error(), "claude-code") {
		t.Errorf("err = %v, should name the runtime that was required", err)
	}
}

func TestHostedOnlyStrategy(t *testing.T) {
	res, err := Resolve(Policy{Strategy: StrategyHosted, Model: ModelAuto},
		Env{Available: []string{"claude-code"}, Task: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Runtime != Hosted {
		t.Errorf("runtime = %q, want hosted", res.Runtime)
	}
}

// The resolution records what was ASKED for as well as what it got, which is
// what makes "why did this run on Codex" answerable months later.
func TestResolutionIsExplainable(t *testing.T) {
	g := grant(t, "codex", ScopeTask, "nightly", "")
	res, err := Resolve(Policy{Strategy: StrategyPreferAuthorized,
		Allowed: []string{"codex", Hosted}, Model: ModelAuto},
		Env{Available: []string{"codex"}, Authorizations: Authorizations{g}, Task: "nightly"})
	if err != nil {
		t.Fatal(err)
	}
	if res.RequestedStrategy != StrategyPreferAuthorized || res.RequestedModel != ModelAuto {
		t.Error("the request must be recorded alongside the result")
	}
	if res.Runtime != "codex" || res.CredentialSource != "codex" {
		t.Errorf("resolved = %q / %q", res.Runtime, res.CredentialSource)
	}
	if res.Model == "" || res.Model == ModelAuto {
		t.Errorf("model = %q, want auto resolved to a concrete default", res.Model)
	}
	if res.AuthID != g.ID || res.AuthScope != ScopeTask {
		t.Errorf("auth = %q/%q, want the grant it relied on", res.AuthID, res.AuthScope)
	}
	if res.Why == "" {
		t.Error("a resolution should say why in one line")
	}
}

// FALLBACK is a list of alternates for RUNTIME failure, drawn only from what is
// authorized and capable.
func TestChainIsAuthorizedAndExcludesTheChoice(t *testing.T) {
	env := Env{
		Available:      []string{"codex", "claude-code"},
		Authorizations: Authorizations{grant(t, "codex", ScopeAll, "", "")},
		Task:           "t",
	}
	chain := Chain(Policy{Model: ModelAuto,
		Fallback: []string{"codex", "claude-code", Hosted}}, env, "codex")
	for _, id := range chain {
		if id == "codex" {
			t.Error("the chosen runtime must not appear in its own fallback chain")
		}
		if id == "claude-code" {
			t.Error("an unauthorized runtime must never be a fallback")
		}
	}
	if len(chain) != 1 || chain[0] != Hosted {
		t.Errorf("chain = %v, want just the hosted gateway", chain)
	}
}

// Detect must only ever report presence.
func TestDetectReportsPresenceOnly(t *testing.T) {
	got := Detect()
	for _, id := range got {
		if _, ok := Get(id); !ok {
			t.Errorf("Detect returned unknown runtime %q", id)
		}
	}
	// Whatever is installed, presence alone authorizes nothing.
	var none Authorizations
	for _, id := range got {
		if id == Hosted {
			continue
		}
		if none.Authorized(id, "t", "") {
			t.Errorf("%q is authorized with no grants recorded", id)
		}
	}
}

func TestZeroMarginalCostIsASubscriptionProperty(t *testing.T) {
	for _, r := range All() {
		if r.Kind == KindSubscription && !r.ZeroMarginalCost {
			t.Errorf("%s is a subscription but not zero-marginal-cost", r.ID)
		}
		if r.Kind == KindHosted && r.ZeroMarginalCost {
			t.Errorf("%s is metered and must not claim zero marginal cost", r.ID)
		}
	}
}

func asNoRuntime(err error, target *ErrNoRuntime) bool {
	if e, ok := err.(ErrNoRuntime); ok {
		*target = e
		return true
	}
	return false
}

// A task-scoped grant is real permission even when the caller has no task in
// hand. Conflating "not authorized for THIS task" with "not authorized at all"
// told a user to authorize something they had already authorized.
func TestAnyGrantSeesNarrowPermissions(t *testing.T) {
	auth := Authorizations{grant(t, "claude-code", ScopeTask, "nightly", "")}

	if _, ok := auth.AnyGrant("claude-code"); !ok {
		t.Error("a task-scoped grant is an authorization")
	}
	if auth.Authorized("claude-code", "other", "") {
		t.Error("but it must not authorize a different task")
	}
	// With no task in hand, nothing is offerable: it is already permitted.
	for _, id := range auth.Offerable("") {
		if id == "claude-code" {
			t.Error("an already-authorized runtime must not be offered again")
		}
	}
	// A revoked grant is not a grant.
	revoked, _ := auth.Revoke(auth[0].ID)
	if _, ok := revoked.AnyGrant("claude-code"); ok {
		t.Error("a revoked grant must not count")
	}
}
