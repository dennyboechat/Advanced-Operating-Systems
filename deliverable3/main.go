// Deliverable 3 - memory management and process synchronization.
//
// The program has three subcommands:
//
//	sched  CPU scheduling from deliverable 2 (round robin, preemptive priority)
//	mem    demand paging over fixed-size page frames, with FIFO or LRU page
//	       replacement, page-fault counting and per-process memory usage
//	sync   semaphores and mutexes: a race-condition demo, producer-consumer on
//	       a bounded buffer, and the dining philosophers without deadlock
//
// With no subcommand the arguments go to sched, so deliverable 2 command lines
// keep working unchanged.
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "sched":
			runSched(args[1:])
			return
		case "mem", "memory":
			runMem(args[1:])
			return
		case "sync":
			runSync(args[1:])
			return
		case "help", "-h", "-help", "--help":
			usage()
			return
		}
	}
	runSched(args)
}

func usage() {
	name := filepath.Base(os.Args[0])
	fmt.Printf("Usage: %s <command> [flags]\n\n", name)
	fmt.Println("Commands:")
	fmt.Println("  sched   CPU scheduling: round robin or preemptive priority (deliverable 2)")
	fmt.Println("  mem     paging: page frames, page faults, FIFO / LRU page replacement")
	fmt.Println("  sync    synchronization: race condition, producer-consumer, dining philosophers")
	fmt.Printf("\nRun \"%s <command> -h\" for the flags of a command.\n", name)
	fmt.Println("\nExamples:")
	fmt.Printf("  %s sched -algo rr -q 3 -file processes.txt\n", name)
	fmt.Printf("  %s mem -algo lru -frames 4 -file workload.txt\n", name)
	fmt.Printf("  %s mem -algo both -frames 3 -refs \"7 0 1 2 0 3 0 4 2 3 0 3 2 1 2 0 1 7 0 1\"\n", name)
	fmt.Printf("  %s mem -i -frames 4\n", name)
	fmt.Printf("  %s sync race\n", name)
	fmt.Printf("  %s sync pc -producers 2 -consumers 3 -buffer 4\n", name)
	fmt.Printf("  %s sync dining -strategy ordered\n", name)
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}
