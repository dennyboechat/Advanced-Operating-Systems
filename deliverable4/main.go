// Deliverable 4 - a shell with pipes, user accounts and file permissions.
//
// With no arguments aos starts the shell: a login prompt, then a Unix-like
// command line with pipelines (ls | grep txt), job control, users with
// different permission levels and a file system with owners and rwx modes.
//
// The operating system simulations of the earlier deliverables are still here,
// as subcommands and as commands inside the shell:
//
//	sched  CPU scheduling from deliverable 2 (round robin, preemptive priority)
//	mem    demand paging with FIFO or LRU page replacement (deliverable 3)
//	sync   semaphores and mutexes: race condition, producer-consumer, dining
//	       philosophers (deliverable 3)
//
// Arguments that are not a subcommand go to sched, so deliverable 2 command
// lines (aos -algo rr -q 3 ...) keep working unchanged.
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		runShell(nil)
		return
	}
	switch args[0] {
	case "shell", "sh", "login":
		runShell(args[1:])
	case "help", "-h", "-help", "--help":
		usage()
	default:
		if !runSubcommand(args[0], args[1:]) {
			runSched(args)
		}
	}
}

// runSubcommand runs one of the simulations. The shell starts them as child
// processes of this same executable.
func runSubcommand(name string, args []string) bool {
	switch name {
	case "sched":
		runSched(args)
	case "mem", "memory":
		runMem(args)
	case "sync":
		runSync(args)
	default:
		return false
	}
	return true
}

func usage() {
	name := filepath.Base(os.Args[0])
	fmt.Printf("Usage: %s [command] [flags]\n\n", name)
	fmt.Println("Commands:")
	fmt.Println("  shell   (default) log in and use the shell: pipes, users, file permissions")
	fmt.Println("  sched   CPU scheduling: round robin or preemptive priority (deliverable 2)")
	fmt.Println("  mem     paging: page frames, page faults, FIFO / LRU page replacement")
	fmt.Println("  sync    synchronization: race condition, producer-consumer, dining philosophers")
	fmt.Printf("\nRun \"%s <command> -h\" for the flags of a command.\n", name)
	fmt.Println("\nExamples:")
	fmt.Printf("  %s                                   log in (accounts: admin, alice, bob, guest)\n", name)
	fmt.Printf("  %s shell -user alice demo.gosh       run a script as alice\n", name)
	fmt.Printf("  %s sched -algo rr -q 3 -file processes.txt\n", name)
	fmt.Printf("  %s mem -algo lru -frames 4 -file workload.txt\n", name)
	fmt.Printf("  %s sync dining -strategy ordered\n", name)
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}
