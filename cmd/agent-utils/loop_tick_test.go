package main

import "testing"

// `loop tick --force` is the only way Deps.Force is ever set. If the flag stops
// being registered the override silently becomes unreachable, and the tick that
// an operator asked for would keep honouring the cooldown without saying so.
func TestLoopTickHasForceFlag(t *testing.T) {
	for _, c := range loopCommand().Commands {
		if c.Name != "tick" {
			continue
		}
		for _, f := range c.Flags {
			for _, n := range f.Names() {
				if n == "force" {
					return
				}
			}
		}
		t.Fatalf("`loop tick` has no --force flag; flags = %v", c.Flags)
	}
	t.Fatal("no `tick` subcommand under `loop`")
}
