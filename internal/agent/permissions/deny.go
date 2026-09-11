package permissions

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// A DENYLIST is the capability half of the permission system: not "ask a human
// about this" but "this run does not have this power at all".
//
// It exists because mode alone cannot express a capability ceiling. An
// unattended run needs Medium actions (a build, a test) to work, and Medium is
// also where `git push` lives — so no single mode both runs the tests and
// withholds the push. Risk answers "how dangerous is this?"; a denylist answers
// "is this run allowed to do this kind of thing?", and the two are independent.
//
// Matching walks the shell AST, exactly like the risk classifier, so `eval`,
// `&&`, pipelines and subshells cannot smuggle a denied command past it. A
// substring check over the raw string would be trivially defeated.

// DeniedBy reports the first command inside `command` that matches any of the
// deny patterns, and what it matched.
//
// A pattern is a space-separated token list where "*" matches any single token:
//
//	"git push"        denies `git push`, `git push -u origin x`
//	"git * --force"   denies a forced push under any subcommand
//	"gh"              denies every gh invocation
//
// Tokens match the command's argv PREFIX, with the leading binary compared by
// basename so /usr/bin/git and git are the same thing.
func DeniedBy(command string, patterns []string) (matched string, denied bool) {
	return deniedBy(command, patterns, 0)
}

// denyDepth bounds wrapper unwrapping, matching the risk classifier's own limit.
const denyDepth = 4

func deniedBy(command string, patterns []string, depth int) (matched string, denied bool) {
	if len(patterns) == 0 || depth > denyDepth {
		return "", false
	}
	f, err := parse(command)
	if err != nil {
		// Unparseable input is refused rather than allowed: a denylist that fails
		// open is not a ceiling. Callers only reach here with a real shell string.
		return command, true
	}
	syntax.Walk(f, func(n syntax.Node) bool {
		if denied {
			return false
		}
		st, ok := n.(*syntax.Stmt)
		if !ok {
			return true
		}
		ce, ok := st.Cmd.(*syntax.CallExpr)
		if !ok || len(ce.Args) == 0 {
			return true
		}
		argv := wordsToArgv(ce.Args)
		if len(argv) == 0 {
			return true
		}
		// Compare the binary by basename; the rest verbatim.
		head := argv[0]
		if i := strings.LastIndexByte(head, '/'); i >= 0 {
			head = head[i+1:]
		}
		norm := append([]string{head}, argv[1:]...)
		for _, p := range patterns {
			if argvMatches(norm, strings.Fields(p)) {
				matched, denied = strings.Join(argv, " "), true
				return false
			}
		}
		// Unwrap the wrappers that RUN something else — eval, sudo, sh -c,
		// timeout, xargs — and check what they would actually run. Reuses the
		// risk classifier's own unwrapper so the denylist and the risk ladder
		// cannot disagree about what a command really is.
		if inner, ok := innerCommand(head, argv[1:], norm); ok && strings.TrimSpace(inner) != "" {
			if m, d := deniedBy(inner, patterns, depth+1); d {
				matched, denied = m, true
				return false
			}
		}
		return true
	})
	return matched, denied
}

// argvMatches reports whether argv starts with the pattern tokens.
func argvMatches(argv, pattern []string) bool {
	if len(pattern) == 0 || len(argv) < len(pattern) {
		return false
	}
	for i, tok := range pattern {
		if tok == "*" {
			continue
		}
		if !strings.EqualFold(argv[i], tok) {
			return false
		}
	}
	return true
}
