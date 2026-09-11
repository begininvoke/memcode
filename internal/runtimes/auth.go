package runtimes

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Authorization is the user saying yes, once, on the record.
//
// It exists as its own object rather than a boolean because "may memcode use my
// Claude Code login" has an obvious follow-up question — for what? — and the
// answer differs. Someone happy to let one nightly task spend their quota is
// not necessarily happy to let every task they ever create do it.

// Scope is how far one grant reaches.
type Scope string

const (
	// ScopeRun authorizes exactly one execution. Of limited use to a scheduler,
	// which is precisely why it is worth supporting: an interactive "just this
	// once" must not silently become standing permission.
	ScopeRun Scope = "run"
	// ScopeTask authorizes one named task, forever. The useful default for
	// unattended work: this job may use my subscription, others may not.
	ScopeTask Scope = "task"
	// ScopeAll authorizes every autonomous task.
	ScopeAll Scope = "all"
)

// ValidScope reports whether s names a real scope.
func ValidScope(s Scope) bool {
	switch s {
	case ScopeRun, ScopeTask, ScopeAll:
		return true
	}
	return false
}

// Grant is one recorded authorization.
type Grant struct {
	ID      string `yaml:"id" json:"id"`
	Runtime string `yaml:"runtime" json:"runtime"`
	Scope   Scope  `yaml:"scope" json:"scope"`
	// Task is required for ScopeTask and meaningless otherwise.
	Task string `yaml:"task,omitempty" json:"task,omitempty"`
	// Run is required for ScopeRun.
	Run       string `yaml:"run,omitempty" json:"run,omitempty"`
	GrantedAt string `yaml:"granted_at" json:"granted_at"`
	// Revoked keeps a withdrawn grant on the record rather than deleting it, so
	// "when did this stop being allowed" stays answerable.
	Revoked bool `yaml:"revoked,omitempty" json:"revoked,omitempty"`
}

// NewGrant records an authorization.
func NewGrant(runtime string, scope Scope, taskName, runID string, now time.Time) (Grant, error) {
	if _, ok := Get(runtime); !ok {
		return Grant{}, fmt.Errorf("unknown runtime %q (known: %s)", runtime, strings.Join(IDs(), ", "))
	}
	if !ValidScope(scope) {
		return Grant{}, fmt.Errorf("unknown scope %q (use run, task or all)", scope)
	}
	if scope == ScopeTask && strings.TrimSpace(taskName) == "" {
		return Grant{}, fmt.Errorf("a task-scoped authorization needs a task name")
	}
	if scope == ScopeRun && strings.TrimSpace(runID) == "" {
		return Grant{}, fmt.Errorf("a run-scoped authorization needs a run id")
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	return Grant{
		ID:        "auth_" + hex.EncodeToString(b[:]),
		Runtime:   runtime,
		Scope:     scope,
		Task:      strings.TrimSpace(taskName),
		Run:       strings.TrimSpace(runID),
		GrantedAt: now.UTC().Format(time.RFC3339),
	}, nil
}

// Covers reports whether this grant authorizes a runtime for a given task/run.
func (g Grant) Covers(runtime, taskName, runID string) bool {
	if g.Revoked || g.Runtime != runtime {
		return false
	}
	switch g.Scope {
	case ScopeAll:
		return true
	case ScopeTask:
		return g.Task != "" && g.Task == taskName
	case ScopeRun:
		return g.Run != "" && g.Run == runID
	}
	return false
}

// Authorizations is the set of grants in force.
type Authorizations []Grant

// Find returns the grant that authorizes a runtime, preferring the NARROWEST
// one that applies. A run-scoped grant is reported over a blanket one so the
// record says which permission was actually relied on, which is the question
// someone asks when they want to withdraw it.
func (a Authorizations) Find(runtime, taskName, runID string) (Grant, bool) {
	var best Grant
	found := false
	rank := map[Scope]int{ScopeRun: 0, ScopeTask: 1, ScopeAll: 2}
	for _, g := range a {
		if !g.Covers(runtime, taskName, runID) {
			continue
		}
		if !found || rank[g.Scope] < rank[best.Scope] {
			best, found = g, true
		}
	}
	return best, found
}

// Authorized reports whether a runtime may be used for this task/run.
//
// The hosted gateway is always authorized: it is memcode's own metered service,
// not a credential borrowed from another tool, and the user's memcode account
// IS the permission.
func (a Authorizations) Authorized(runtime, taskName, runID string) bool {
	if runtime == Hosted {
		return true
	}
	_, ok := a.Find(runtime, taskName, runID)
	return ok
}

// Revoke marks matching grants withdrawn and reports how many changed.
func (a Authorizations) Revoke(id string) (Authorizations, int) {
	n := 0
	out := make(Authorizations, len(a))
	copy(out, a)
	for i := range out {
		if out[i].ID == id && !out[i].Revoked {
			out[i].Revoked = true
			n++
		}
	}
	return out, n
}

// AnyGrant reports whether a runtime has ANY active authorization, of any
// scope. Distinct from Authorized, which answers a specific task: a
// task-scoped grant is real permission even when the caller has no task in
// hand, and treating it as none would tell a user to authorize something they
// already did.
func (a Authorizations) AnyGrant(runtime string) (Grant, bool) {
	for _, g := range a {
		if g.Runtime == runtime && !g.Revoked {
			return g, true
		}
	}
	return Grant{}, false
}

// Offerable lists runtimes present on this machine with NO authorization at
// all — the set actually worth offering. A task name narrows it to those that
// cannot serve THAT task; empty means "anything entirely unauthorized".
func (a Authorizations) Offerable(taskName string) []string {
	var out []string
	for _, id := range Detect() {
		if id == Hosted {
			continue
		}
		if taskName == "" {
			if _, ok := a.AnyGrant(id); !ok {
				out = append(out, id)
			}
			continue
		}
		if !a.Authorized(id, taskName, "") {
			out = append(out, id)
		}
	}
	return out
}
