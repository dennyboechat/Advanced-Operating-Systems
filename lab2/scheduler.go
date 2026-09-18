package main

import (
	"fmt"
	"sort"
	"time"
)

// Policy selects a scheduling algorithm.
type Policy int

const (
	PolicyRoundRobin Policy = iota
	PolicyPriority
)

func (p Policy) String() string {
	if p == PolicyPriority {
		return "priority (preemptive)"
	}
	return "round robin"
}

// ParsePolicy turns the -algo flag value into a policy.
func ParsePolicy(s string) (Policy, error) {
	switch s {
	case "rr", "round-robin", "roundrobin":
		return PolicyRoundRobin, nil
	case "priority", "prio":
		return PolicyPriority, nil
	default:
		return 0, fmt.Errorf("unknown algorithm %q (use \"rr\" or \"priority\")", s)
	}
}

// Process is a job submitted to the scheduler.
type Process struct {
	Name     string // process identifier, e.g. "P1"
	Arrival  int    // time unit at which the process enters the system
	Burst    int    // total CPU time the process needs
	Priority int    // scheduling priority, interpreted by a PriorityRule
}

// SliceKind tells what the CPU was doing during a timeline entry.
type SliceKind int

const (
	KindRun    SliceKind = iota // a process held the CPU
	KindIdle                    // no process was ready
	KindSwitch                  // context-switch overhead
)

// TimeSlice is one contiguous interval of the simulated CPU timeline.
type TimeSlice struct {
	Kind      SliceKind
	Name      string // process name for KindRun, empty otherwise
	Start     int    // inclusive
	End       int    // exclusive
	Remaining int    // CPU time the process still needs when the slice ended
	Completed bool   // the process finished exactly at End
	Preemptor string // process that took the CPU away, if any

	Wall time.Duration // real time the slice took, when the run was live
}

// Duration is the length of the slice in time units.
func (s TimeSlice) Duration() int { return s.End - s.Start }

// Stats holds the per-process results of a simulation run.
type Stats struct {
	Process
	Completion  int // time unit at which the process finished
	Turnaround  int // Completion - Arrival
	Waiting     int // Turnaround - Burst
	Response    int // first time on the CPU - Arrival
	Preemptions int // times the process lost the CPU before it was done
}

// Result is everything a run of a scheduler produced.
type Result struct {
	Algorithm     string
	Quantum       int // round robin only
	Rule          PriorityRule
	ContextSwitch int
	Timeline      []TimeSlice
	Stats         []Stats // in the order the processes were given
	Makespan      int     // time unit at which the last process finished
	BusyTime      int     // time units spent executing processes
	IdleTime      int     // time units with an empty ready queue
	SwitchTime    int     // time units lost to context switches

	Live bool          // the run was executed in real time by RunLive
	Tick time.Duration // real duration of one time unit, when live
	Wall time.Duration // real duration of the whole run, when live
}

// AvgTurnaround returns the mean turnaround time over all processes.
func (r Result) AvgTurnaround() float64 {
	return mean(r.Stats, func(s Stats) int { return s.Turnaround })
}

// AvgWaiting returns the mean waiting time over all processes.
func (r Result) AvgWaiting() float64 { return mean(r.Stats, func(s Stats) int { return s.Waiting }) }

// AvgResponse returns the mean response time over all processes.
func (r Result) AvgResponse() float64 { return mean(r.Stats, func(s Stats) int { return s.Response }) }

// Utilization is the fraction of the makespan spent running processes.
func (r Result) Utilization() float64 {
	if r.Makespan == 0 {
		return 0
	}
	return float64(r.BusyTime) / float64(r.Makespan) * 100
}

// Throughput is the number of processes completed per time unit.
func (r Result) Throughput() float64 {
	if r.Makespan == 0 {
		return 0
	}
	return float64(len(r.Stats)) / float64(r.Makespan)
}

func mean(stats []Stats, field func(Stats) int) float64 {
	if len(stats) == 0 {
		return 0
	}
	sum := 0
	for _, s := range stats {
		sum += field(s)
	}
	return float64(sum) / float64(len(stats))
}

// validate rejects process sets and costs the schedulers cannot simulate.
func validate(procs []Process, contextSwitch int) error {
	if contextSwitch < 0 {
		return fmt.Errorf("context switch cost cannot be negative, got %d", contextSwitch)
	}
	if len(procs) == 0 {
		return fmt.Errorf("no processes to schedule")
	}
	for _, p := range procs {
		if p.Arrival < 0 {
			return fmt.Errorf("process %s: arrival time cannot be negative", p.Name)
		}
		if p.Burst < 1 {
			return fmt.Errorf("process %s: burst time must be at least 1", p.Name)
		}
	}
	return nil
}

// arrivalOrder returns the process indices sorted by arrival time, ties broken
// by the order the processes were submitted (first come, first served).
func arrivalOrder(procs []Process) []int {
	order := make([]int, len(procs))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return procs[order[a]].Arrival < procs[order[b]].Arrival
	})
	return order
}

// RoundRobin simulates round-robin scheduling of procs with the given time
// slice. Every dispatched process runs for at most quantum time units; when the
// slice expires it goes to the back of the ready queue and the next process is
// scheduled. A process that finishes before its slice is over leaves the queue
// immediately and the CPU is handed to the next process right away.
//
// contextSwitch is the overhead charged whenever the CPU is handed from one
// process to a different one (0 disables it). Processes that arrive while a
// process is running join the ready queue before the preempted process, which
// is the usual convention for this algorithm.
func RoundRobin(procs []Process, quantum, contextSwitch int) (Result, error) {
	if quantum < 1 {
		return Result{}, fmt.Errorf("time slice must be at least 1, got %d", quantum)
	}
	if err := validate(procs, contextSwitch); err != nil {
		return Result{}, err
	}

	result := Result{Algorithm: PolicyRoundRobin.String(), Quantum: quantum, ContextSwitch: contextSwitch}
	result.Stats = make([]Stats, len(procs))

	remaining := make([]int, len(procs))
	for i, p := range procs {
		remaining[i] = p.Burst
		result.Stats[i] = Stats{Process: p, Response: -1}
	}

	order := arrivalOrder(procs)

	var queue []int // indices into procs; front of the ready queue is queue[0]
	arrived := 0    // how many entries of order are already in the queue
	now := 0
	running := -1 // process that held the CPU last, for context-switch accounting
	done := 0

	// admit moves every process that has arrived by time t into the ready queue.
	admit := func(t int) {
		for arrived < len(order) && procs[order[arrived]].Arrival <= t {
			queue = append(queue, order[arrived])
			arrived++
		}
	}

	admit(now)

	for done < len(procs) {
		if len(queue) == 0 {
			// Nothing is ready: the CPU idles until the next arrival.
			next := procs[order[arrived]].Arrival
			result.Timeline = append(result.Timeline, TimeSlice{
				Kind: KindIdle, Start: now, End: next,
			})
			result.IdleTime += next - now
			now = next
			admit(now)
			continue
		}

		current := queue[0]
		queue = queue[1:]

		if running != -1 && running != current && contextSwitch > 0 {
			result.Timeline = append(result.Timeline, TimeSlice{
				Kind: KindSwitch, Start: now, End: now + contextSwitch,
			})
			result.SwitchTime += contextSwitch
			now += contextSwitch
			admit(now)
		}

		if result.Stats[current].Response < 0 {
			result.Stats[current].Response = now - procs[current].Arrival
		}

		run := quantum
		if remaining[current] < run {
			run = remaining[current]
		}
		start := now
		now += run
		remaining[current] -= run
		result.BusyTime += run

		// Processes that arrived during the slice queue up ahead of the
		// process that was just preempted.
		admit(now)

		completed := remaining[current] == 0
		if completed {
			done++
			result.Stats[current].Completion = now
			result.Stats[current].Turnaround = now - procs[current].Arrival
			result.Stats[current].Waiting = result.Stats[current].Turnaround - procs[current].Burst
		} else {
			result.Stats[current].Preemptions++
			queue = append(queue, current)
		}

		result.Timeline = append(result.Timeline, TimeSlice{
			Kind:      KindRun,
			Name:      procs[current].Name,
			Start:     start,
			End:       now,
			Remaining: remaining[current],
			Completed: completed,
		})
		running = current
	}

	result.Makespan = now
	return result, nil
}
