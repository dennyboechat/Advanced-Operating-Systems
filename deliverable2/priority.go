package main

import (
	"container/heap"
	"fmt"
)

// PriorityRule says how the priority number of a process is read.
type PriorityRule int

const (
	// LowNumberFirst is the usual Unix convention: priority 0 outranks 1.
	LowNumberFirst PriorityRule = iota
	// HighNumberFirst treats the larger number as the more urgent process.
	HighNumberFirst
)

func (r PriorityRule) String() string {
	if r == HighNumberFirst {
		return "higher number = higher priority"
	}
	return "lower number = higher priority"
}

// outranks reports whether priority a is strictly more urgent than b.
func (r PriorityRule) outranks(a, b int) bool {
	if r == HighNumberFirst {
		return a > b
	}
	return a < b
}

// pcb is a process control block as held by the ready queue.
type pcb struct {
	idx      int // index into the caller's process slice
	priority int
	arrival  int
	seq      int // submission order, the final tie-break
}

// readyQueue is a binary heap (container/heap) ordered by priority, then by
// arrival time and submission order so that processes of equal priority are
// served first come, first served. It implements heap.Interface.
type readyQueue struct {
	items []*pcb
	rule  PriorityRule
}

func (q *readyQueue) Len() int { return len(q.items) }

func (q *readyQueue) Less(i, j int) bool {
	a, b := q.items[i], q.items[j]
	if a.priority != b.priority {
		return q.rule.outranks(a.priority, b.priority)
	}
	if a.arrival != b.arrival {
		return a.arrival < b.arrival // FCFS within one priority level
	}
	return a.seq < b.seq
}

func (q *readyQueue) Swap(i, j int) { q.items[i], q.items[j] = q.items[j], q.items[i] }

func (q *readyQueue) Push(x any) { q.items = append(q.items, x.(*pcb)) }

func (q *readyQueue) Pop() any {
	last := len(q.items) - 1
	item := q.items[last]
	q.items[last] = nil
	q.items = q.items[:last]
	return item
}

// Peek returns the most urgent process without removing it from the queue.
func (q *readyQueue) Peek() *pcb { return q.items[0] }

// PriorityScheduling simulates preemptive priority scheduling. The most urgent
// ready process always holds the CPU; processes of equal priority are served in
// arrival order (FCFS). When a process with a strictly higher priority arrives,
// the running process is preempted at that instant, goes back into the ready
// queue with its remaining time, and the newcomer is dispatched.
//
// The ready queue is a binary heap, so selecting the next process is O(log n)
// rather than a scan of every ready process.
//
// contextSwitch is the overhead charged whenever the CPU is handed to a
// different process (0 disables it).
func PriorityScheduling(procs []Process, rule PriorityRule, contextSwitch int) (Result, error) {
	if err := validate(procs, contextSwitch); err != nil {
		return Result{}, err
	}

	result := Result{
		Algorithm:     PolicyPriority.String(),
		Rule:          rule,
		ContextSwitch: contextSwitch,
		Stats:         make([]Stats, len(procs)),
	}

	remaining := make([]int, len(procs))
	for i, p := range procs {
		remaining[i] = p.Burst
		result.Stats[i] = Stats{Process: p, Response: -1}
	}

	order := arrivalOrder(procs)
	queue := &readyQueue{rule: rule}
	heap.Init(queue)

	arrived := 0  // how many entries of order are already in the queue
	now := 0      // current time unit
	running := -1 // process that held the CPU last
	paid := false // a context switch was charged and not yet consumed
	done := 0

	// admit moves every process that has arrived by time t into the queue.
	admit := func(t int) {
		for arrived < len(order) && procs[order[arrived]].Arrival <= t {
			i := order[arrived]
			heap.Push(queue, &pcb{idx: i, priority: procs[i].Priority, arrival: procs[i].Arrival, seq: i})
			arrived++
		}
	}

	admit(now)

	for done < len(procs) {
		if queue.Len() == 0 {
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

		if !paid && contextSwitch > 0 && running != -1 && running != queue.Peek().idx {
			result.Timeline = append(result.Timeline, TimeSlice{
				Kind: KindSwitch, Start: now, End: now + contextSwitch,
			})
			result.SwitchTime += contextSwitch
			now += contextSwitch
			admit(now)
			// A process may have arrived during the switch; whoever is the
			// most urgent now is dispatched, and pays nothing more.
			paid = true
			continue
		}

		current := heap.Pop(queue).(*pcb)
		paid = false
		idx := current.idx

		if result.Stats[idx].Response < 0 {
			result.Stats[idx].Response = now - procs[idx].Arrival
		}

		// The process runs to completion unless a process with a strictly
		// higher priority arrives first. Nothing already in the queue can
		// preempt it: it was the most urgent process when it was dispatched.
		end := now + remaining[idx]
		preemptor := ""
		for k := arrived; k < len(order); k++ {
			next := procs[order[k]]
			if next.Arrival >= end {
				break
			}
			if rule.outranks(next.Priority, procs[idx].Priority) {
				end = next.Arrival
				preemptor = next.Name
				break
			}
		}

		start := now
		now = end
		remaining[idx] -= end - start
		result.BusyTime += end - start

		admit(now)

		completed := remaining[idx] == 0
		if completed {
			done++
			result.Stats[idx].Completion = now
			result.Stats[idx].Turnaround = now - procs[idx].Arrival
			result.Stats[idx].Waiting = result.Stats[idx].Turnaround - procs[idx].Burst
		} else {
			result.Stats[idx].Preemptions++
			heap.Push(queue, current)
		}

		result.Timeline = append(result.Timeline, TimeSlice{
			Kind:      KindRun,
			Name:      procs[idx].Name,
			Start:     start,
			End:       now,
			Remaining: remaining[idx],
			Completed: completed,
			Preemptor: preemptor,
		})
		running = idx
	}

	result.Makespan = now
	return result, nil
}

// ParsePriorityRule turns the -priority flag value into a rule.
func ParsePriorityRule(s string) (PriorityRule, error) {
	switch s {
	case "low", "low-number", "lowest":
		return LowNumberFirst, nil
	case "high", "high-number", "highest":
		return HighNumberFirst, nil
	default:
		return 0, fmt.Errorf("unknown priority rule %q (use \"low\" or \"high\")", s)
	}
}
