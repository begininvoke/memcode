package runtimes

import (
	"fmt"
	"strings"
)

// Strategy is how a task wants its runtime chosen.
type Strategy string

const (
	// StrategyPreferAuthorized walks Allowed in order and takes the first
	// authorized, capable, zero-marginal-cost runtime, falling through to the
	// hosted gateway when none qualifies.
	//
	// Note what this is NOT: a cost optimizer. "Cheapest" stops being meaningful
	// once latency, model quality, tool support, context limits and subscription
	// quota differ, and a hidden optimizer picking differently on different days
	// is unpredictable in exactly the place that most wants to be boring. The
	// order in Allowed is the user's preference, and it is obeyed.
	StrategyPreferAuthorized Strategy = "prefer_authorized_subscription"
	// StrategyHosted always uses memcode's gateway, whatever else is available.
	StrategyHosted Strategy = "hosted_only"
	// StrategyExplicit requires the first entry in Allowed and refuses to
	// substitute. A task that must run on Codex says so and fails loudly rather
	// than quietly running somewhere else.
	StrategyExplicit Strategy = "explicit"
)

// ValidStrategy reports whether s names a real strategy.
func ValidStrategy(s Strategy) bool {
	switch s {
	case StrategyPreferAuthorized, StrategyHosted, StrategyExplicit:
		return true
	}
	return false
}

// Strategies lists the valid strategies, for error messages.
func Strategies() []Strategy {
	return []Strategy{StrategyPreferAuthorized, StrategyHosted, StrategyExplicit}
}

// Policy is a task's runtime preference, as authored.
type Policy struct {
	Strategy Strategy
	// Allowed is the ordered preference list. Order is meaningful.
	Allowed []string
	// Model is a catalog model id, or "auto".
	Model string
	// Fallback names runtimes that may be tried when the SELECTED runtime fails
	// as a runtime — not when the task fails. See Chain.
	Fallback []string
}

// Env is the machine's answer to "what is available and what is allowed".
type Env struct {
	Available      []string
	Authorizations Authorizations
	Task           string
	Run            string
}

func (e Env) available(id string) bool {
	for _, a := range e.Available {
		if a == id {
			return true
		}
	}
	return false
}

// Resolution is the frozen decision, recorded on the run so it stays
// explainable long after the machine's state has changed.
type Resolution struct {
	// What was asked for.
	RequestedStrategy Strategy
	RequestedModel    string
	// What it resolved to.
	Runtime          string
	Model            string
	CredentialSource string
	// Which permission was relied on. Empty for the hosted gateway, which needs
	// none.
	AuthID    string
	AuthScope Scope
	// Chain is the ordered alternates for RUNTIME failure only.
	Chain []string
	// Why explains the choice in one line, for the run record.
	Why string
}

// ErrNoRuntime means nothing authorized and capable remains.
type ErrNoRuntime struct {
	Reason string
	// Offer lists runtimes that are present but unauthorized — the actionable
	// part, because the fix is usually one authorization away.
	Offer []string
}

func (e ErrNoRuntime) Error() string { return e.Reason }

// Resolve picks the runtime for a task, deterministically.
//
// The order is fixed and stated, because an unattended system that chooses
// differently on different days for reasons nobody can reconstruct is worse
// than one that chooses slightly wrong every time:
//
//  1. an explicit runtime, if authorized and capable
//  2. the authorized, capable entries of Allowed, in the user's order
//  3. the hosted gateway, if the policy permits it
//  4. nothing — blocked, with the unauthorized candidates named
func Resolve(p Policy, env Env) (Resolution, error) {
	need := Need{Model: p.Model}
	res := Resolution{RequestedStrategy: p.Strategy, RequestedModel: p.Model}

	usable := func(id string) (Runtime, string, bool) {
		r, ok := Get(id)
		if !ok {
			return Runtime{}, "unknown runtime", false
		}
		if r.Kind != KindHosted && !env.available(id) {
			return r, "not present on this machine", false
		}
		if !env.Authorizations.Authorized(id, env.Task, env.Run) {
			return r, "not authorized for autonomous tasks", false
		}
		if !r.CanServe(need) {
			return r, fmt.Sprintf("cannot serve model %q", p.Model), false
		}
		return r, "", true
	}

	switch p.Strategy {
	case StrategyHosted:
		r, why, ok := usable(Hosted)
		if !ok {
			return Resolution{}, ErrNoRuntime{Reason: "hosted runtime unusable: " + why}
		}
		return finish(res, r, need, env, "policy is hosted_only"), nil

	case StrategyExplicit:
		if len(p.Allowed) == 0 {
			return Resolution{}, ErrNoRuntime{Reason: "strategy is explicit but runtime.allowed is empty"}
		}
		id := p.Allowed[0]
		r, why, ok := usable(id)
		if !ok {
			// An explicit choice NEVER substitutes. Quietly running somewhere
			// else would defeat the only reason to pin a runtime.
			return Resolution{}, ErrNoRuntime{
				Reason: fmt.Sprintf("runtime %q was required but is %s", id, why),
				Offer:  offerFor(id, env),
			}
		}
		return finish(res, r, need, env, "explicitly required by the task"), nil
	}

	// prefer_authorized_subscription (the default).
	var tried []string
	for _, id := range p.Allowed {
		r, why, ok := usable(id)
		if !ok {
			tried = append(tried, fmt.Sprintf("%s (%s)", id, why))
			continue
		}
		if r.Kind == KindHosted {
			// Reached in the preference list rather than as a fallback: honour
			// it, but say so plainly.
			return finish(res, r, need, env, "first authorized runtime in the task's order"), nil
		}
		return finish(res, r, need, env, "preferred authorized subscription"), nil
	}

	if allowsHosted(p) {
		if r, why, ok := usable(Hosted); ok {
			return finish(res, r, need, env, "no authorized subscription runtime; fell back to hosted"), nil
		} else if why != "" {
			tried = append(tried, fmt.Sprintf("%s (%s)", Hosted, why))
		}
	}

	reason := "no authorized, capable runtime"
	if len(tried) > 0 {
		reason += ": " + strings.Join(tried, "; ")
	}
	return Resolution{}, ErrNoRuntime{Reason: reason, Offer: env.Authorizations.Offerable(env.Task)}
}

// allowsHosted reports whether the policy permits the hosted gateway at all,
// whether named in the preference list or in the fallback list.
func allowsHosted(p Policy) bool {
	for _, id := range append(append([]string{}, p.Allowed...), p.Fallback...) {
		if id == Hosted {
			return true
		}
	}
	return false
}

// offerFor names a runtime worth authorizing, when THAT is what is missing.
func offerFor(id string, env Env) []string {
	if env.available(id) && !env.Authorizations.Authorized(id, env.Task, env.Run) {
		return []string{id}
	}
	return nil
}

func finish(res Resolution, r Runtime, need Need, env Env, why string) Resolution {
	res.Runtime = r.ID
	res.Model = r.ResolveModel(need)
	res.CredentialSource = r.CredentialSource
	res.Why = why
	if g, ok := env.Authorizations.Find(r.ID, env.Task, env.Run); ok {
		res.AuthID, res.AuthScope = g.ID, g.Scope
	}
	return res
}

// Chain returns the alternates that may be attempted when the RESOLVED runtime
// fails as a runtime.
//
// The distinction this enforces is the important one. A runtime that is
// unreachable, unauthenticated or broken is a reason to try another; a runtime
// that worked perfectly and produced a change whose tests failed is NOT. Handing
// the same codebase to a different model in the hope of a different answer is
// not fallback, it is rerolling — and it turns one honest failure into a silent
// search for a model that happens to agree.
func Chain(p Policy, env Env, chosen string) []string {
	need := Need{Model: p.Model}
	var out []string
	seen := map[string]bool{chosen: true}
	for _, id := range p.Fallback {
		if seen[id] {
			continue
		}
		r, ok := Get(id)
		if !ok {
			continue
		}
		if r.Kind != KindHosted && !env.available(id) {
			continue
		}
		if !env.Authorizations.Authorized(id, env.Task, env.Run) || !r.CanServe(need) {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
