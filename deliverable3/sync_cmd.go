package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// runSync is the "sync" subcommand. Its first argument picks the experiment.
func runSync(args []string) {
	if len(args) == 0 {
		syncUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "race":
		runRace(args[1:])
	case "pc", "producer-consumer":
		runProducerConsumer(args[1:])
	case "dining", "philosophers":
		runDining(args[1:])
	case "-h", "-help", "--help", "help":
		syncUsage()
	default:
		fmt.Fprintf(os.Stderr, "error: unknown sync experiment %q\n\n", args[0])
		syncUsage()
		os.Exit(2)
	}
}

func syncUsage() {
	name := filepath.Base(os.Args[0]) + " sync"
	fmt.Printf("Usage: %s <experiment> [flags]\n\n", name)
	fmt.Println("Experiments:")
	fmt.Println("  race     processes increment a shared counter, without and then with a mutex")
	fmt.Println("  pc       producer-consumer on a bounded buffer guarded by semaphores")
	fmt.Println("  dining   dining philosophers, with deadlock prevented (or not, with -strategy naive)")
	fmt.Printf("\nRun \"%s <experiment> -h\" for its flags.\n", name)
}

func runRace(args []string) {
	fs := flag.NewFlagSet("sync race", flag.ExitOnError)
	procs := fs.Int("procs", 4, "processes sharing the counter")
	iters := fs.Int("iters", 1000, "increments per process")
	fs.Parse(args)

	fmt.Printf("Race condition on shared memory | %d processes x %d increments of one shared counter\n\n", *procs, *iters)
	fmt.Println("Mode             Expected      Final       Lost   Mutex waits   Time")
	for _, synchronized := range []bool{false, true} {
		r, err := SharedCounter(*procs, *iters, synchronized)
		if err != nil {
			fail(err)
		}
		mode := "no lock"
		if synchronized {
			mode = "mutex"
		}
		fmt.Printf("%-14s %10d %10d %10d %13d   %v\n",
			mode, r.Expected, r.Got, r.Lost(), r.Blocks, r.Wall.Round(time.Millisecond))
	}
	fmt.Println()
	fmt.Println("Without a lock, processes read the same value and overwrite each other's")
	fmt.Println("increment. With the read-modify-write inside a mutex-guarded critical")
	fmt.Println("section, only one process updates the counter at a time and nothing is lost.")
}

func runProducerConsumer(args []string) {
	fs := flag.NewFlagSet("sync pc", flag.ExitOnError)
	cfg := PCConfig{}
	fs.IntVar(&cfg.Producers, "producers", 2, "number of producers")
	fs.IntVar(&cfg.Consumers, "consumers", 2, "number of consumers")
	fs.IntVar(&cfg.Capacity, "buffer", 3, "bounded buffer capacity")
	fs.IntVar(&cfg.Items, "items", 5, "items made by each producer")
	fs.DurationVar(&cfg.Delay, "delay", 50*time.Millisecond, "upper bound of the random time to produce or consume an item")
	fs.Int64Var(&cfg.Seed, "seed", time.Now().UnixNano(), "random seed for the delays")
	quiet := fs.Bool("quiet", false, "print only the summary")
	fs.Parse(args)

	fmt.Printf("Producer-consumer | %d producer(s), %d consumer(s), buffer of %d, %d item(s) per producer\n\n",
		cfg.Producers, cfg.Consumers, cfg.Capacity, cfg.Items)
	if !*quiet {
		cfg.Log = liveLogger()
	}
	r, err := ProducerConsumer(cfg)
	if err != nil {
		fail(err)
	}
	if !*quiet {
		fmt.Println()
	}

	total := 0
	for p, n := range r.Produced {
		fmt.Printf("Producer %d   produced %d\n", p+1, n)
		total += n
	}
	consumed := 0
	for c, n := range r.Consumed {
		fmt.Printf("Consumer %d   consumed %d\n", c+1, n)
		consumed += n
	}
	missing, duplicated := checkDelivery(r)
	fmt.Printf("\nItems produced / consumed:   %d / %d (missing %d, duplicated %d)\n", total, consumed, missing, duplicated)
	fmt.Printf("Peak buffer occupancy:       %d of %d\n", r.MaxOccupancy, cfg.Capacity)
	fmt.Printf("Producer waits (full):       %d\n", r.FullWaits)
	fmt.Printf("Consumer waits (empty):      %d\n", r.EmptyWaits)
	fmt.Printf("Overflows / underflows:      %d / %d\n", r.Overflows, r.Underflows)
	fmt.Printf("Real time elapsed:           %v\n", r.Wall.Round(time.Millisecond))
}

// checkDelivery counts produced items that no consumer received and items
// received more than once. Both are 0 when the buffer is correctly synchronized.
func checkDelivery(r PCResult) (missing, duplicated int) {
	seen := make(map[Item]int)
	for _, items := range r.Received {
		for _, it := range items {
			seen[it]++
		}
	}
	for p, n := range r.Produced {
		for seq := 1; seq <= n; seq++ {
			switch c := seen[Item{Producer: p + 1, Seq: seq}]; {
			case c == 0:
				missing++
			case c > 1:
				duplicated += c - 1
			}
		}
	}
	return missing, duplicated
}

func runDining(args []string) {
	fs := flag.NewFlagSet("sync dining", flag.ExitOnError)
	cfg := DiningConfig{}
	strategy := fs.String("strategy", "ordered", "\"ordered\" (lower fork first), \"waiter\" (at most N-1 seated), or \"naive\" (left then right, deadlocks)")
	fs.IntVar(&cfg.Philosophers, "n", 5, "number of philosophers (and forks)")
	fs.IntVar(&cfg.Meals, "meals", 3, "meals each philosopher eats")
	fs.DurationVar(&cfg.Think, "think", 50*time.Millisecond, "upper bound of a random thinking time")
	fs.DurationVar(&cfg.Eat, "eat", 50*time.Millisecond, "upper bound of a random eating time")
	fs.DurationVar(&cfg.Reach, "reach", 10*time.Millisecond, "pause between picking up the first and the second fork")
	fs.Int64Var(&cfg.Seed, "seed", time.Now().UnixNano(), "random seed for the delays")
	quiet := fs.Bool("quiet", false, "print only the summary")
	fs.Parse(args)

	s, err := ParseDiningStrategy(*strategy)
	if err != nil {
		fail(err)
	}
	cfg.Strategy = s

	fmt.Printf("Dining philosophers | %d philosophers, %d meal(s) each, strategy %s\n\n", cfg.Philosophers, cfg.Meals, s)
	if !*quiet {
		cfg.Log = liveLogger()
	}
	r, err := DiningPhilosophers(cfg)
	if err != nil {
		fail(err)
	}
	if !*quiet {
		fmt.Println()
	}

	for i, n := range r.Meals {
		fmt.Printf("Philosopher %d   ate %d of %d meal(s)\n", i, n, cfg.Meals)
	}
	fmt.Println()
	if r.Deadlocked {
		fmt.Println("Result:                      DEADLOCK (every philosopher held one fork and waited for another)")
	} else {
		fmt.Println("Result:                      every philosopher finished, no deadlock")
	}
	fmt.Printf("Most eating at once:         %d (at most %d possible)\n", r.MaxEating, cfg.Philosophers/2)
	fmt.Printf("Neighbours eating together:  %d (must be 0)\n", r.NeighborViolations)
	fmt.Printf("Waits for a fork:            %d\n", r.ForkWaits)
	fmt.Printf("Real time elapsed:           %v\n", r.Wall.Round(time.Millisecond))
}
