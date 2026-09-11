package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
	"github.com/memcode-ai/memcode/internal/runtimes"
)

// Authorizing a runtime is a deliberate, recorded act with a stated reach. It
// is a separate command rather than a prompt during a run because an unattended
// scheduler has nobody to prompt, and because "yes" to a dialog at 3am is not a
// thing that can happen.

// loadAuthorizations reads the machine's recorded runtime permissions.
func loadAuthorizations() runtimes.Authorizations {
	// Only the authorization section has to make sense. A stale agent stanza
	// elsewhere in gateway.yaml is someone else'''s migration, not a reason this
	// command cannot tell you what is authorized.
	s, err := gwconfig.LoadFor(gwconfig.SectionRuntimeAuth)
	if err != nil {
		return nil
	}
	out := make(runtimes.Authorizations, 0, len(s.AuthorizedRuntimes))
	for _, g := range s.AuthorizedRuntimes {
		out = append(out, runtimes.Grant{
			ID: g.ID, Runtime: g.Runtime, Scope: runtimes.Scope(g.Scope),
			Task: g.Task, Run: g.Run, GrantedAt: g.GrantedAt, Revoked: g.Revoked,
		})
	}
	return out
}

func saveAuthorizations(a runtimes.Authorizations) error {
	s, err := gwconfig.LoadFor(gwconfig.SectionRuntimeAuth)
	if err != nil {
		return err
	}
	s.AuthorizedRuntimes = s.AuthorizedRuntimes[:0]
	for _, g := range a {
		s.AuthorizedRuntimes = append(s.AuthorizedRuntimes, gwconfig.RuntimeGrant{
			ID: g.ID, Runtime: g.Runtime, Scope: string(g.Scope),
			Task: g.Task, Run: g.Run, GrantedAt: g.GrantedAt, Revoked: g.Revoked,
		})
	}
	// Sections this command did not touch round-trip verbatim.
	return gwconfig.SaveFor(s, gwconfig.SectionRuntimeAuth)
}

var taskRuntimeCmd = &cobra.Command{
	Use:     "runtime",
	Aliases: []string{"runtimes"},
	Short:   "See and authorize where autonomous tasks run",
	Long: `An autonomous task runs its inference somewhere: a subscription you already hold
(Claude Code, Codex, Copilot, Grok) or memcode's hosted gateway.

A subscription being installed on this machine does NOT mean tasks may use it. You signed
into that tool, not into a scheduler that would spend your quota while you sleep, so a
subscription needs an explicit authorization before any unattended task can reach it. The
hosted gateway needs none: it is memcode's own metered service.`,
	RunE: func(cmd *cobra.Command, args []string) error { return taskRuntimeListCmd.RunE(cmd, args) },
}

var taskRuntimeListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "Show runtimes, what is installed, and what is authorized",
	RunE: func(cmd *cobra.Command, args []string) error {
		auth := loadAuthorizations()
		present := map[string]bool{}
		for _, id := range runtimes.Detect() {
			present[id] = true
		}
		fmt.Printf("  %-16s %-12s %-11s %s\n", "RUNTIME", "INSTALLED", "AUTHORIZED", "")
		for _, r := range runtimes.All() {
			installed := "no"
			if r.Kind == runtimes.KindHosted || present[r.ID] {
				installed = "yes"
			}
			state := "no"
			switch {
			case r.Kind == runtimes.KindHosted:
				state = "n/a"
			default:
				if g, ok := auth.AnyGrant(r.ID); ok {
					state = string(g.Scope)
					if g.Task != "" {
						state += ":" + g.Task
					}
				}
			}
			fmt.Printf("  %-16s %-12s %-11s %s\n", r.ID, installed, state, r.Display)
		}
		if offer := auth.Offerable(""); len(offer) > 0 {
			fmt.Printf("\nInstalled but not authorized: %s\n", strings.Join(offer, ", "))
			fmt.Printf("Authorize one with `memcode task runtime authorize <runtime> --scope all`.\n")
		}
		return nil
	},
}

var taskRuntimeAuthorizeCmd = &cobra.Command{
	Use:   "authorize <runtime>",
	Short: "Allow autonomous tasks to use a runtime",
	Long: `Records permission for unattended tasks to run on a runtime you already pay for.

Scope decides how far the permission reaches:

  --scope all           every autonomous task
  --scope task <name>   one named task only
  --scope run <id>      a single run

Nothing is authorized by default, however obviously installed it is.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		scope, _ := cmd.Flags().GetString("scope")
		taskName, _ := cmd.Flags().GetString("task")
		runID, _ := cmd.Flags().GetString("run")

		rt, ok := runtimes.Get(args[0])
		if !ok {
			return fmt.Errorf("unknown runtime %q (known: %s)", args[0], strings.Join(runtimes.IDs(), ", "))
		}
		if rt.Kind == runtimes.KindHosted {
			return fmt.Errorf("%s needs no authorization — it is memcode's own metered service", rt.ID)
		}
		if rt.Available != nil && !rt.Available() {
			// Authorizing something absent is almost always a typo, and the
			// permission would sit there doing nothing until someone noticed.
			return fmt.Errorf("%s is not installed on this machine — sign into it first, then authorize it", rt.ID)
		}
		g, err := runtimes.NewGrant(rt.ID, runtimes.Scope(scope), taskName, runID, time.Now())
		if err != nil {
			return err
		}
		auth := append(loadAuthorizations(), g)
		if err := saveAuthorizations(auth); err != nil {
			return err
		}
		switch g.Scope {
		case runtimes.ScopeAll:
			fmt.Printf("%s authorized for all autonomous tasks (%s)\n", rt.Display, g.ID)
		case runtimes.ScopeTask:
			fmt.Printf("%s authorized for task %q (%s)\n", rt.Display, g.Task, g.ID)
		default:
			fmt.Printf("%s authorized for run %s (%s)\n", rt.Display, g.Run, g.ID)
		}
		return nil
	},
}

var taskRuntimeRevokeCmd = &cobra.Command{
	Use:   "revoke <authorization-id>",
	Short: "Withdraw a runtime authorization",
	Long: `Marks an authorization withdrawn. The record is kept rather than deleted, so
"when did this stop being allowed" stays answerable.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		auth, n := loadAuthorizations().Revoke(args[0])
		if n == 0 {
			return fmt.Errorf("no active authorization %q", args[0])
		}
		if err := saveAuthorizations(auth); err != nil {
			return err
		}
		fmt.Printf("revoked %s\n", args[0])
		return nil
	},
}

var taskRuntimeGrantsCmd = &cobra.Command{
	Use:   "grants",
	Short: "List every authorization on the record, including withdrawn ones",
	RunE: func(cmd *cobra.Command, args []string) error {
		auth := loadAuthorizations()
		if len(auth) == 0 {
			fmt.Println("No runtime authorizations. Autonomous tasks use the hosted gateway.")
			return nil
		}
		for _, g := range auth {
			state := "active"
			if g.Revoked {
				state = "revoked"
			}
			target := string(g.Scope)
			if g.Task != "" {
				target += " " + g.Task
			}
			if g.Run != "" {
				target += " " + g.Run
			}
			fmt.Printf("  %-18s %-14s %-20s %-8s %s\n", g.ID, g.Runtime, target, state, g.GrantedAt)
		}
		return nil
	},
}

func init() {
	taskRuntimeAuthorizeCmd.Flags().String("scope", string(runtimes.ScopeAll), "how far the permission reaches: all, task, run")
	taskRuntimeAuthorizeCmd.Flags().String("task", "", "task name, for --scope task")
	taskRuntimeAuthorizeCmd.Flags().String("run", "", "run id, for --scope run")
	taskRuntimeCmd.AddCommand(taskRuntimeListCmd, taskRuntimeAuthorizeCmd, taskRuntimeRevokeCmd, taskRuntimeGrantsCmd)
	taskCmd.AddCommand(taskRuntimeCmd)
}
