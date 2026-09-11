package task

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/atomicfile"
	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
	yaml "go.yaml.in/yaml/v4"
)

// Tasks live in two roots:
//
//	<repo>/.memcode/tasks/<name>.yaml   project scope — travels with the repo
//	~/.config/memcode/tasks/<name>.yaml global scope — per machine
//
// Project scope wins a name collision: a repo that ships a task means it for
// that repo. The project root is committable on purpose (see
// config.EnsureGitignore, which carves tasks/ out of .memcode's ignore-all), so
// a team can review an autonomous task in a pull request like any other code.

// DirName is the tasks subdirectory inside either root.
const DirName = "tasks"

// ProjectDir returns the repo-local task directory.
func ProjectDir(root string) string {
	return filepath.Join(root, ".memcode", DirName)
}

// GlobalDir returns the per-machine task directory.
func GlobalDir() (string, error) {
	dir, err := gwconfig.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, DirName), nil
}

// ErrNotFound is returned by Get when no task carries that name.
var ErrNotFound = errors.New("task not found")

// Parse decodes one task file. Decoding is STRICT — an unknown key is an error
// rather than a silently ignored line, because a typo'd `autonmy:` that parses
// cleanly would hand a task the default authority while its author believed
// otherwise.
func Parse(data []byte, path string, scope Scope, now time.Time) (Task, error) {
	var t Task
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&t); err != nil {
		return Task{}, fmt.Errorf("%s: %w", path, err)
	}
	t.ApplyDefaults()
	t.Path, t.Scope = path, scope
	if err := t.Validate(now); err != nil {
		return Task{}, fmt.Errorf("%s: %w", path, err)
	}
	return t, nil
}

// Marshal renders a task back to YAML, defaults applied. Used when memcode
// writes a task it proposed, so the file on disk is the complete definition
// rather than a sparse one whose behaviour shifts if a default ever changes.
func Marshal(t Task) ([]byte, error) {
	t.ApplyDefaults()
	t.Path, t.Scope = "", ""
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(t); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// loadDir reads every *.yaml in dir. A single malformed file is reported but
// never stops the others from loading: one bad task must not take the whole
// scheduler down with it.
func loadDir(dir string, scope Scope, now time.Time) ([]Task, []error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil // a missing tasks dir is the normal case, not an error
	}
	var out []Task
	var errs []error
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		t, err := Parse(data, path, scope, now)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		// The filename is the addressable identity; a name that disagrees with
		// it makes `task run <name>` ambiguous against the file you edited.
		if base := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())); base != t.Name {
			errs = append(errs, fmt.Errorf("%s: name %q does not match the filename (rename the file to %s.yaml)", path, t.Name, t.Name))
			continue
		}
		out = append(out, t)
	}
	return out, errs
}

// Load returns every task visible from root, project scope shadowing global on
// a name collision, sorted by name. Malformed files are returned separately so
// a caller can surface them without losing the tasks that did load.
func Load(root string, now time.Time) ([]Task, []error) {
	var errs []error
	byName := map[string]Task{}

	if dir, err := GlobalDir(); err == nil {
		tasks, e := loadDir(dir, ScopeGlobal, now)
		errs = append(errs, e...)
		for _, t := range tasks {
			byName[t.Name] = t
		}
	}
	if root != "" {
		tasks, e := loadDir(ProjectDir(root), ScopeProject, now)
		errs = append(errs, e...)
		for _, t := range tasks {
			byName[t.Name] = t // project wins
		}
	}

	out := make([]Task, 0, len(byName))
	for _, t := range byName {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
	return out, errs
}

// Get resolves one task by name, honouring the same precedence as Load.
func Get(root, name string, now time.Time) (Task, error) {
	tasks, errs := Load(root, now)
	for _, t := range tasks {
		if t.Name == name {
			return t, nil
		}
	}
	// A task that exists but failed to parse deserves its parse error, not a
	// bare "not found" that sends the user looking for a missing file.
	for _, err := range errs {
		if strings.Contains(err.Error(), string(filepath.Separator)+name+".y") {
			return Task{}, err
		}
	}
	return Task{}, fmt.Errorf("%w: %s", ErrNotFound, name)
}

// Save writes a task to the chosen scope's directory, creating it if needed.
// The write is atomic so a crash mid-save cannot leave the scheduler reading a
// truncated definition.
func Save(root string, t Task, scope Scope) (string, error) {
	t.ApplyDefaults()
	if err := t.Validate(time.Now()); err != nil {
		return "", err
	}
	var dir string
	switch scope {
	case ScopeProject:
		if root == "" {
			return "", fmt.Errorf("no project root for a project-scoped task")
		}
		dir = ProjectDir(root)
	case ScopeGlobal:
		d, err := GlobalDir()
		if err != nil {
			return "", err
		}
		dir = d
	default:
		return "", fmt.Errorf("unknown scope %q", scope)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	data, err := Marshal(t)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, t.Name+".yaml")
	if err := atomicfile.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// ResolveProject returns the absolute, symlink-resolved directory a task runs
// in: its own project field when set, else the root it was loaded from.
func (t Task) ResolveProject(root string) (string, error) {
	if strings.TrimSpace(t.Project) != "" {
		return gwconfig.CanonicalRoot(t.Project)
	}
	if root == "" {
		return "", fmt.Errorf("task %q has no project and no root was supplied", t.Name)
	}
	return gwconfig.CanonicalRoot(root)
}
