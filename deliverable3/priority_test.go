package main

import (
	"container/heap"
	"testing"
)

// A higher-priority arrival must take the CPU away from the running process at
// the instant it arrives.
func TestPriorityPreemptsRunningProcess(t *testing.T) {
	procs := []Process{
		{"P1", 0, 6, 3}, // starts, is preempted twice
		{"P2", 2, 4, 1}, // more urgent than P1
		{"P3", 3, 2, 0}, // most urgent of all
	}
	r, err := PriorityScheduling(procs, LowNumberFirst, 0)
	if err != nil {
		t.Fatalf("PriorityScheduling: %v", err)
	}

	want := "P1[0,2) P2[2,3) P3[3,5) P2[5,8) P1[8,12)"
	if got := gantt(r); got != want {
		t.Errorf("timeline\n got %s\nwant %s", got, want)
	}

	cases := []struct {
		completion, turnaround, waiting, response, preemptions int
	}{
		{12, 12, 6, 0, 1}, // P1 preempted by P2
		{8, 6, 2, 0, 1},   // P2 preempted by P3
		{5, 2, 0, 0, 0},   // P3 runs straight through
	}
	for i, want := range cases {
		s := r.Stats[i]
		if s.Completion != want.completion || s.Turnaround != want.turnaround ||
			s.Waiting != want.waiting || s.Response != want.response ||
			s.Preemptions != want.preemptions {
			t.Errorf("%s: got completion=%d turnaround=%d waiting=%d response=%d preemptions=%d, want %+v",
				s.Name, s.Completion, s.Turnaround, s.Waiting, s.Response, s.Preemptions, want)
		}
	}

	// The preemption is recorded with the process that caused it.
	if r.Timeline[0].Preemptor != "P2" {
		t.Errorf("P1 preempted by %q, want P2", r.Timeline[0].Preemptor)
	}
	if r.Timeline[1].Preemptor != "P3" {
		t.Errorf("P2 preempted by %q, want P3", r.Timeline[1].Preemptor)
	}
}

// Processes of equal priority are served first come, first served, and an
// arrival of equal priority never preempts.
func TestEqualPrioritiesAreFCFS(t *testing.T) {
	procs := []Process{
		{"P1", 0, 3, 1},
		{"P2", 1, 3, 1},
		{"P3", 2, 3, 1},
	}
	r, err := PriorityScheduling(procs, LowNumberFirst, 0)
	if err != nil {
		t.Fatalf("PriorityScheduling: %v", err)
	}
	want := "P1[0,3) P2[3,6) P3[6,9)"
	if got := gantt(r); got != want {
		t.Errorf("timeline\n got %s\nwant %s", got, want)
	}
	for _, s := range r.Stats {
		if s.Preemptions != 0 {
			t.Errorf("%s was preempted by a process of equal priority", s.Name)
		}
	}
}

// Processes that are already waiting are picked most-urgent-first, and equal
// priorities keep their arrival order.
func TestSelectionOrderAmongWaitingProcesses(t *testing.T) {
	procs := []Process{
		{"P1", 0, 2, 5}, // runs first, nothing else has arrived
		{"P2", 0, 2, 2},
		{"P3", 0, 2, 2},
		{"P4", 0, 2, 1},
	}
	// Everything arrives at 0, so P1 is dispatched first only if it is the
	// most urgent; it is not, so the heap order decides the whole run.
	r, err := PriorityScheduling(procs, LowNumberFirst, 0)
	if err != nil {
		t.Fatalf("PriorityScheduling: %v", err)
	}
	want := "P4[0,2) P2[2,4) P3[4,6) P1[6,8)"
	if got := gantt(r); got != want {
		t.Errorf("timeline\n got %s\nwant %s", got, want)
	}
}

func TestPriorityHighNumberWins(t *testing.T) {
	procs := []Process{
		{"P1", 0, 4, 1},
		{"P2", 1, 2, 9},
	}
	r, err := PriorityScheduling(procs, HighNumberFirst, 0)
	if err != nil {
		t.Fatalf("PriorityScheduling: %v", err)
	}
	want := "P1[0,1) P2[1,3) P1[3,6)"
	if got := gantt(r); got != want {
		t.Errorf("timeline\n got %s\nwant %s", got, want)
	}

	// The same input under the default rule never preempts.
	r, err = PriorityScheduling(procs, LowNumberFirst, 0)
	if err != nil {
		t.Fatalf("PriorityScheduling: %v", err)
	}
	if want, got := "P1[0,4) P2[4,6)", gantt(r); got != want {
		t.Errorf("timeline\n got %s\nwant %s", got, want)
	}
}

func TestPriorityIdleCPU(t *testing.T) {
	procs := []Process{{"P1", 0, 2, 1}, {"P2", 6, 2, 0}}
	r, err := PriorityScheduling(procs, LowNumberFirst, 0)
	if err != nil {
		t.Fatalf("PriorityScheduling: %v", err)
	}
	want := "P1[0,2) idle[2,6) P2[6,8)"
	if got := gantt(r); got != want {
		t.Errorf("timeline\n got %s\nwant %s", got, want)
	}
	if r.IdleTime != 4 || r.BusyTime != 4 || r.Makespan != 8 {
		t.Errorf("got idle=%d busy=%d makespan=%d, want 4/4/8", r.IdleTime, r.BusyTime, r.Makespan)
	}
}

func TestPriorityContextSwitchOverhead(t *testing.T) {
	procs := []Process{{"P1", 0, 4, 2}, {"P2", 2, 2, 0}}
	r, err := PriorityScheduling(procs, LowNumberFirst, 1)
	if err != nil {
		t.Fatalf("PriorityScheduling: %v", err)
	}
	// P1 runs 0-2, is preempted by P2, one switch each way.
	want := "P1[0,2) cs[2,3) P2[3,5) cs[5,6) P1[6,8)"
	if got := gantt(r); got != want {
		t.Errorf("timeline\n got %s\nwant %s", got, want)
	}
	if r.SwitchTime != 2 || r.BusyTime != 6 || r.Makespan != 8 {
		t.Errorf("got switch=%d busy=%d makespan=%d, want 2/6/8", r.SwitchTime, r.BusyTime, r.Makespan)
	}
}

// Every process must run for exactly its burst, and no process may start
// before it arrives, whatever the priority layout.
func TestPriorityInvariants(t *testing.T) {
	procs := []Process{
		{"P1", 0, 5, 4},
		{"P2", 1, 3, 4},
		{"P3", 2, 6, 0},
		{"P4", 7, 2, 2},
		{"P5", 15, 4, 1},
	}
	for _, rule := range []PriorityRule{LowNumberFirst, HighNumberFirst} {
		for cs := 0; cs <= 2; cs++ {
			r, err := PriorityScheduling(procs, rule, cs)
			if err != nil {
				t.Fatalf("rule %v, cs %d: %v", rule, cs, err)
			}

			ran := map[string]int{}
			for _, s := range r.Timeline {
				if s.Kind == KindRun {
					ran[s.Name] += s.Duration()
				}
			}
			for _, p := range procs {
				if ran[p.Name] != p.Burst {
					t.Errorf("rule %v, cs %d: %s ran %d unit(s), want %d", rule, cs, p.Name, ran[p.Name], p.Burst)
				}
			}
			for _, s := range r.Stats {
				if s.Response < 0 || s.Waiting < 0 {
					t.Errorf("rule %v, cs %d: %s has negative waiting/response time", rule, cs, s.Name)
				}
				if s.Completion-s.Arrival != s.Turnaround {
					t.Errorf("rule %v, cs %d: %s turnaround is inconsistent", rule, cs, s.Name)
				}
			}
			if r.BusyTime+r.IdleTime+r.SwitchTime != r.Makespan {
				t.Errorf("rule %v, cs %d: busy+idle+switch = %d, want makespan %d",
					rule, cs, r.BusyTime+r.IdleTime+r.SwitchTime, r.Makespan)
			}
		}
	}
}

func TestPriorityInvalidInput(t *testing.T) {
	cases := []struct {
		name          string
		procs         []Process
		contextSwitch int
	}{
		{"no processes", nil, 0},
		{"zero burst", []Process{{"P1", 0, 0, 1}}, 0},
		{"negative arrival", []Process{{"P1", -1, 3, 1}}, 0},
		{"negative context switch", []Process{{"P1", 0, 3, 1}}, -1},
	}
	for _, tc := range cases {
		if _, err := PriorityScheduling(tc.procs, LowNumberFirst, tc.contextSwitch); err == nil {
			t.Errorf("%s: expected an error, got none", tc.name)
		}
	}
}

// The ready queue must hand out processes most-urgent-first, then by arrival,
// then by submission order, whatever order they were pushed in.
func TestReadyQueueOrdering(t *testing.T) {
	q := &readyQueue{rule: LowNumberFirst}
	heap.Init(q)
	pushed := []*pcb{
		{idx: 0, priority: 2, arrival: 5, seq: 0},
		{idx: 1, priority: 0, arrival: 9, seq: 1},
		{idx: 2, priority: 2, arrival: 1, seq: 2},
		{idx: 3, priority: 1, arrival: 4, seq: 3},
		{idx: 4, priority: 2, arrival: 1, seq: 4}, // same priority and arrival as idx 2
	}
	for _, p := range pushed {
		heap.Push(q, p)
	}
	if got := q.Peek().idx; got != 1 {
		t.Errorf("Peek returned process %d, want 1", got)
	}

	want := []int{1, 3, 2, 4, 0}
	for _, w := range want {
		got := heap.Pop(q).(*pcb).idx
		if got != w {
			t.Errorf("popped process %d, want %d", got, w)
		}
	}
	if q.Len() != 0 {
		t.Errorf("queue still holds %d process(es)", q.Len())
	}
}

func TestParsePriorityRule(t *testing.T) {
	for _, s := range []string{"low", "low-number", "lowest"} {
		if r, err := ParsePriorityRule(s); err != nil || r != LowNumberFirst {
			t.Errorf("ParsePriorityRule(%q) = %v, %v", s, r, err)
		}
	}
	for _, s := range []string{"high", "high-number", "highest"} {
		if r, err := ParsePriorityRule(s); err != nil || r != HighNumberFirst {
			t.Errorf("ParsePriorityRule(%q) = %v, %v", s, r, err)
		}
	}
	if _, err := ParsePriorityRule("sideways"); err == nil {
		t.Error("ParsePriorityRule(\"sideways\"): expected an error, got none")
	}
}
