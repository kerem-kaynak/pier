// pier — coding-agent sessions as parked-when-idle micro-VMs on your own cloud.
//
//	pier                    open the TUI (list / attach / new / delete)
//	pier <branch> [base]    create a session from the current repo and attach
//	pier ls                 list sessions (scriptable)
//	pier attach <match>     reattach (starts a parked session)
//	pier rm <match>         destroy session (instance + disk)
//	pier keep <match>       pin: disable auto-park for this session
//	pier setup              one-time wizard (per machine; per account if first)
//	pier bake               build the prebaked session image (fast creates)
//	pier doctor             environment + account checks
//	pier teardown           remove the per-account groundwork
package main

import (
	"fmt"
	"os"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		fail("TUI not implemented yet — coming with the session store")
	}
	switch args[0] {
	case "setup", "ls", "attach", "rm", "keep", "bake", "doctor", "teardown":
		fail(fmt.Sprintf("%q not implemented yet", args[0]))
	case "help", "-h", "--help":
		fmt.Println("usage: pier [<branch> [base] | ls | attach <m> | rm <m> | keep <m> | setup | bake | doctor | teardown]")
	default:
		// Bare name = create a session for that branch from the cwd repo.
		fail(fmt.Sprintf("create %q: not implemented yet", args[0]))
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "pier: "+msg)
	os.Exit(1)
}
