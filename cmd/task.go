package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/task"
)

var taskCmd = &cobra.Command{
	Use:     "task",
	Aliases: []string{"tasks"},
	Short:   "Inspect and run autonomous tasks",
	Long: `A task is a bounded unit of work memcode can run without you watching — refresh a model
catalog, review security advisories, upgrade dependencies — declared in YAML and runnable by
name.

A task is not the same thing as a schedule. It is runnable on demand with ` + "`memcode task run`" + `,
and a trigger is something you attach when you want it to happen on its own.

Definitions live in two places, with the project copy winning a name collision:

  <repo>/.memcode/tasks/<name>.yaml    travels with the repo, reviewable in a PR
  ~/.config/memcode/tasks/<name>.yaml  this machine only`,
	RunE: func(cmd *cobra.Command, args []string) error { return taskListCmd.RunE(cmd, args) },
}

// taskRoot resolves the project root for task lookup. A task can name its own
// project, so this is only the starting point, not necessarily where it runs.
func taskRoot() string {
	root, _, err := config.Resolve(".")
	if err != nil {
		return ""
	}
	return root
}

// reportLoadErrors prints malformed definitions to stderr. They go to stderr,
// and never abort the command, because one bad file must not hide the tasks
// that are fine — the same reason the loader keeps them separate.
func reportLoadErrors(errs []error) {
	for _, err := range errs {
		fmt.Fprintf(os.Stderr, "  ! %v\n", err)
	}
}

// cadence renders a task's triggers for a listing.
func cadence(t task.Task) string {
	if len(t.Triggers) == 0 {
		return "manual"
	}
	parts := make([]string, 0, len(t.Triggers))
	for _, tr := range t.Triggers {
		switch {
		case tr.Manual:
			parts = append(parts, "manual")
		case tr.Cron != "":
			s := tr.Cron
			if tr.TZ != "" {
				s += " " + tr.TZ
			}
			parts = append(parts, s)
		case tr.Every != "":
			parts = append(parts, "every "+tr.Every)
		case tr.At != "":
			parts = append(parts, "at "+tr.At)
		}
	}
	return strings.Join(parts, ", ")
}

// gitShadowsTasks reports the root .gitignore rule that hides .memcode/tasks
// from git, if any. A project task is meant to be committed and reviewed, but
// git never descends into an ignored directory, so a repo-wide `.memcode/` rule
// silently wins over the exception memcode writes inside it.
//
// This is REPORTED, never fixed: the root .gitignore is a file the user curates,
// and EnsureGitignore's whole contract is that memcode does not edit it. Saying
// so plainly beats either editing it behind their back or letting them believe a
// task is committed when it is not.
func gitShadowsTasks(root string) (rule string, shadowed bool) {
	if root == "" {
		return "", false
	}
	dir := filepath.Join(root, ".memcode", "tasks")
	if _, err := os.Stat(dir); err != nil {
		return "", false
	}
	if _, err := exec.LookPath("git"); err != nil {
		return "", false
	}
	// check-ignore exits 0 when the path IS ignored, naming the winning rule.
	cmd := exec.Command("git", "-C", root, "check-ignore", "-v", filepath.Join(dir, ".probe.yaml"))
	out, err := cmd.Output()
	if err != nil {
		return "", false // not ignored, or not a git repo
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", false
	}
	// "<file>:<line>:<pattern>\t<path>" — the pattern is the useful half.
	if fields := strings.SplitN(line, "\t", 2); len(fields) > 0 {
		return fields[0], true
	}
	return line, true
}

// warnIfShadowed prints the one line a user needs to make project tasks
// committable, when something upstream is hiding them.
func warnIfShadowed(root string) {
	rule, shadowed := gitShadowsTasks(root)
	if !shadowed {
		return
	}
	fmt.Fprintf(os.Stderr, "\n  ! project tasks are not visible to git — %s ignores them.\n", rule)
	fmt.Fprintf(os.Stderr, "    They still run, but they will not travel with the repo or show up in review.\n")
	fmt.Fprintf(os.Stderr, "    To commit them, add this to the repo's .gitignore:  !.memcode/tasks/\n")
}

var taskListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List the tasks visible from this project",
	RunE: func(cmd *cobra.Command, args []string) error {
		tasks, errs := task.Load(taskRoot(), time.Now())
		reportLoadErrors(errs)
		if len(tasks) == 0 {
			fmt.Println("No tasks yet. Write one at .memcode/tasks/<name>.yaml, or ask memcode to")
			fmt.Println("box up a piece of work you expect to repeat.")
			return nil
		}
		warnIfShadowed(taskRoot())
		for _, t := range tasks {
			state := ""
			if !t.IsEnabled() {
				state = " (disabled)"
			}
			fmt.Printf("  %-28s %-22s %-9s %s%s\n",
				t.Name, cadence(t), t.Autonomy.Level, t.Description, state)
		}
		return nil
	},
}

var taskShowCmd = &cobra.Command{
	Use:     "show <name>",
	Aliases: []string{"get"},
	Short:   "Show one task's full definition and resolved authority",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		root := taskRoot()
		t, err := task.Get(root, args[0], time.Now())
		if err != nil {
			return err
		}
		rev, err := t.Revision()
		if err != nil {
			return err
		}
		grants, err := t.Grants()
		if err != nil {
			return err
		}

		fmt.Printf("%s\n", t.Name)
		if t.Description != "" {
			fmt.Printf("  %s\n", t.Description)
		}
		fmt.Printf("\n  file        %s (%s scope)\n", t.Path, t.Scope)
		fmt.Printf("  revision    %s\n", rev)
		fmt.Printf("  enabled     %t\n", t.IsEnabled())
		if project, err := t.ResolveProject(root); err == nil {
			fmt.Printf("  project     %s\n", project)
		}
		fmt.Printf("  cadence     %s\n", cadence(t))
		fmt.Printf("  execution   %s", t.Execution.Mode)
		if t.Execution.Procedure != "" {
			fmt.Printf(" (procedure %s)", t.Execution.Procedure)
		}
		fmt.Printf("\n  runtime     %s / %s\n", t.Agent.Provider, t.Agent.Model)
		fmt.Printf("  timeout     %s\n", t.Timeout())

		// Show the EXPANDED authority, not the tier name. The tier is a label;
		// the grants are what the policy engine actually enforces, and that is
		// what someone auditing an unattended task needs to see.
		fmt.Printf("\n  authority   %s\n", t.Autonomy.Level)
		for _, g := range grants {
			fmt.Printf("                %s\n", g)
		}
		if t.MayOpenPR() {
			fmt.Printf("  pull request %s, branch %s\n", t.Git.PullRequest, t.Git.Branch)
		}
		if len(t.Verify.Commands) > 0 {
			fmt.Printf("\n  verify\n")
			for _, c := range t.Verify.Commands {
				fmt.Printf("                %s\n", c)
			}
		}
		if t.Instructions != "" {
			fmt.Printf("\n  instructions\n")
			for _, line := range strings.Split(strings.TrimRight(t.Instructions, "\n"), "\n") {
				fmt.Printf("    %s\n", line)
			}
		}
		return nil
	},
}

var taskCheckCmd = &cobra.Command{
	Use:   "check",
	Short: "Validate every task definition and report what is wrong",
	Long: `Parses every task file and reports the ones that do not load. Worth running after
hand-editing a definition: the scheduler skips a task it cannot parse, so a file with a typo
is silently not running rather than loudly broken.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		tasks, errs := task.Load(taskRoot(), time.Now())
		reportLoadErrors(errs)
		if len(errs) > 0 {
			return fmt.Errorf("%d task file(s) failed to load", len(errs))
		}
		fmt.Printf("%d task(s) OK\n", len(tasks))
		warnIfShadowed(taskRoot())
		return nil
	},
}

func init() {
	taskCmd.AddCommand(taskListCmd, taskShowCmd, taskCheckCmd)
	rootCmd.AddCommand(taskCmd)
}
