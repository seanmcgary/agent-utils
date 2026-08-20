// Package wizard interactively builds a loop configuration file.
//
// Setting up a new loop today means hand-copying an example YAML and editing
// it, with no check that the result loads until a tick fails on it. This
// package asks for every field of config.Config instead, with a template's
// values offered as defaults, and proves the file it writes actually loads by
// reloading it through config.Load before Write returns.
//
// It is a package on its own, not code embedded in a command, for two
// reasons: two commands need it (`project init` and `project loop new`), and
// a wizard driven only through a terminal cannot be tested. Every question
// goes through the Prompter seam, so a test drives a scripted Prompter and
// never opens a terminal or reads os.Stdin.
package wizard
