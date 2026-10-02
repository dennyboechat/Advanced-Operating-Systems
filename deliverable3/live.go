package main

import (
	"container/heap"
	"fmt"
	"math"
	"time"
)

// LiveConfig configures a real-time run. One simulated time unit lasts Tick of
// real time, so a process with a burst of 3 keeps the CPU busy for 3 ticks.
type LiveConfig struct {
	Policy        Policy
	Quantum       int          // round robin: time slice, in time units
	Rule          PriorityRule // priority: which number wins
	ContextSwitch int          // overhead in time units
	Tick          time.Duration
	Log           func(format string, args ...any) // per-event output, may be nil
}

// report is what a process goroutine sends back when it stops running.
type report struct {
	idx         int
	interrupted bool // the scheduler preempted it before its budget ran out
}

// worker is the goroutine that stands in for one process. It does nothing but
// sleep for as long as the scheduler lets it hold the CPU, which is what makes
// the simulation take real time.
type worker struct {
	idx  int
	run  chan time.Duration // the scheduler grants a slice of this length
	stop chan struct{}      // the scheduler takes the CPU back
}

// serve runs the process until its channel is closed. A granted slice ends
// either when its timer fires or when the scheduler preempts it, whichever
// comes first - this select is the whole of process switching.
func (w *worker) serve(done chan<- report) {
	for budget := range w.run {
		// Drop a preemption signal left over from an earlier slice.
		select {
		case <-w.stop:
		default:
		}

		timer := time.NewTimer(budget)
		select {
		case <-timer.C:
			done <- report{idx: w.idx}
		case <-w.stop:
			timer.Stop()
			done <- report{idx: w.idx, interrupted: true}
		}
	}
}

// RunLive executes the process set in real time. Each process is a goroutine
// that sleeps for the duration it is given; the scheduler hands out the CPU
// with timers, so a round-robin process is switched out when its time slice
// timer fires, and a priority process is switched out the instant a
// higher-priority process arrives.
//
// The result is reported in simulated time units, measured by rounding the
// elapsed real time to the nearest tick.
func RunLive(procs []Process, cfg LiveConfig) (Result, error) {
	if cfg.Tick <= 0 {
		return Result{}, fmt.Errorf("tick must be positive, got %v", cfg.Tick)
	}
	if cfg.Policy == PolicyRoundRobin && cfg.Quantum < 1 {
		return Result{}, fmt.Errorf("time slice must be at least 1, got %d", cfg.Quantum)
	}
	if err := validate(procs, cfg.ContextSwitch); err != nil {
		return Result{}, err
	}

	logf := cfg.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}

	result := Result{
		Algorithm:     cfg.Policy.String(),
		Rule:          cfg.Rule,
		ContextSwitch: cfg.ContextSwitch,
		Live:          true,
		Tick:          cfg.Tick,
		Stats:         make([]Stats, len(procs)),
	}
	if cfg.Policy == PolicyRoundRobin {
		result.Quantum = cfg.Quantum
	}

	remaining := make([]int, len(procs))
	for i, p := range procs {
		remaining[i] = p.Burst
		result.Stats[i] = Stats{Process: p, Response: -1}
	}

	// One goroutine per process.
	done := make(chan report, len(procs))
	workers := make([]*worker, len(procs))
	for i := range procs {
		workers[i] = &worker{idx: i, run: make(chan time.Duration), stop: make(chan struct{}, 1)}
		go workers[i].serve(done)
	}
	defer func() {
		for _, w := range workers {
			close(w.run)
		}
	}()

	// The ready queue: FIFO for round robin, the priority heap otherwise.
	var fifo []int
	pq := &readyQueue{rule: cfg.Rule}
	heap.Init(pq)
	byPriority := cfg.Policy == PolicyPriority

	push := func(idx int) {
		if byPriority {
			heap.Push(pq, &pcb{idx: idx, priority: procs[idx].Priority, arrival: procs[idx].Arrival, seq: idx})
			return
		}
		fifo = append(fifo, idx)
	}
	pop := func() int {
		if byPriority {
			return heap.Pop(pq).(*pcb).idx
		}
		idx := fifo[0]
		fifo = fifo[1:]
		return idx
	}
	peek := func() int {
		if byPriority {
			return pq.Peek().idx
		}
		return fifo[0]
	}
	ready := func() int {
		if byPriority {
			return pq.Len()
		}
		return len(fifo)
	}

	order := arrivalOrder(procs)
	start := time.Now()

	// clock converts elapsed real time into simulated time units.
	clock := func() int {
		return int(math.Round(float64(time.Since(start)) / float64(cfg.Tick)))
	}

	// A timer per arrival, scheduled against the start of the run so that the
	// arrival times cannot drift.
	arrivals := make(chan int, len(procs))
	go func() {
		for _, idx := range order {
			if wait := time.Duration(procs[idx].Arrival)*cfg.Tick - time.Since(start); wait > 0 {
				time.Sleep(wait)
			}
			arrivals <- idx
		}
	}()

	arrived := 0
	accept := func(idx int) {
		push(idx)
		arrived++
		logf("%s arrives (burst %d, priority %d), added to the ready queue",
			procs[idx].Name, procs[idx].Burst, procs[idx].Priority)
	}
	// admit waits for and enqueues every process due to arrive by unit.
	admit := func(unit int) {
		for arrived < len(order) && procs[order[arrived]].Arrival <= unit {
			accept(<-arrivals)
		}
	}

	admit(0)

	running := -1 // process that held the CPU last
	paid := false // a context switch was charged and not yet consumed
	finished := 0
	end := 0 // time unit at which the last slice ended

	for finished < len(procs) {
		if ready() == 0 {
			// Nothing to run: block until the next arrival timer fires.
			from := clock()
			accept(<-arrivals)
			to := clock()
			if to > from {
				result.Timeline = append(result.Timeline, TimeSlice{Kind: KindIdle, Start: from, End: to})
				result.IdleTime += to - from
				logf("CPU idle for %d unit(s), ready queue was empty", to-from)
			}
			continue
		}

		if !paid && cfg.ContextSwitch > 0 && running != -1 && running != peek() {
			from := clock()
			time.Sleep(time.Duration(cfg.ContextSwitch) * cfg.Tick)
			to := clock()
			result.Timeline = append(result.Timeline, TimeSlice{Kind: KindSwitch, Start: from, End: to})
			result.SwitchTime += to - from
			logf("context switch (%d unit(s))", to-from)
			admit(to)
			paid = true
			continue
		}

		idx := pop()
		paid = false

		budget := remaining[idx]
		if !byPriority && cfg.Quantum < budget {
			budget = cfg.Quantum
		}

		from := clock()
		if result.Stats[idx].Response < 0 {
			result.Stats[idx].Response = from - procs[idx].Arrival
		}
		logf("%s dispatched, runs for up to %d unit(s) (%v), %d unit(s) of work left",
			procs[idx].Name, budget, time.Duration(budget)*cfg.Tick, remaining[idx])

		wallStart := time.Now()
		workers[idx].run <- time.Duration(budget) * cfg.Tick

		// Wait for the slice to end: either the process goroutine reports back,
		// or a process arrives. Under priority scheduling an arrival that
		// outranks the running process takes the CPU away immediately.
		preemptor := ""
		var rep report
		for waiting := true; waiting; {
			select {
			case rep = <-done:
				waiting = false
			case a := <-arrivals:
				accept(a)
				if byPriority && cfg.Rule.outranks(procs[a].Priority, procs[idx].Priority) {
					workers[idx].stop <- struct{}{}
					rep = <-done
					preemptor = procs[a].Name
					waiting = false
				}
			}
		}
		wall := time.Since(wallStart)
		if !rep.interrupted {
			// The budget timer won the race against the preemption signal, so
			// the process kept the CPU for the whole slice after all.
			preemptor = ""
		}

		to := clock()
		ran := to - from
		if ran < 0 {
			ran = 0
		}
		remaining[idx] -= ran
		result.BusyTime += ran

		admit(to)

		completed := remaining[idx] == 0
		if completed {
			finished++
			result.Stats[idx].Completion = to
			result.Stats[idx].Turnaround = to - procs[idx].Arrival
			result.Stats[idx].Waiting = result.Stats[idx].Turnaround - procs[idx].Burst
			logf("%s finished after %d unit(s) (%v), removed from the queue", procs[idx].Name, ran, wall.Round(time.Millisecond))
		} else {
			result.Stats[idx].Preemptions++
			push(idx)
			if preemptor != "" {
				logf("%s preempted by %s after %d unit(s) (%v), %d unit(s) left, requeued",
					procs[idx].Name, preemptor, ran, wall.Round(time.Millisecond), remaining[idx])
			} else {
				logf("%s used its whole time slice of %d unit(s) (%v), %d unit(s) left, requeued",
					procs[idx].Name, ran, wall.Round(time.Millisecond), remaining[idx])
			}
		}

		if ran > 0 {
			result.Timeline = append(result.Timeline, TimeSlice{
				Kind:      KindRun,
				Name:      procs[idx].Name,
				Start:     from,
				End:       to,
				Remaining: remaining[idx],
				Completed: completed,
				Preemptor: preemptor,
				Wall:      wall,
			})
		}
		running = idx
		end = to
	}

	result.Makespan = end
	result.Wall = time.Since(start)
	return result, nil
}
