package main

import "testing"

// gantt renders the timeline compactly, e.g. "P1[0,2) P2[2,4)".
func gantt(r Result) string {
	out := ""
	for i, s := range r.Timeline {
		if i > 0 {
			out += " "
		}
		name := s.Name
		switch s.Kind {
		case KindIdle:
			name = "idle"
		case KindSwitch:
			name = "cs"
		}
		out += name + "[" + itoa(s.Start) + "," + itoa(s.End) + ")"
	}
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := ""
	for ; i > 0; i /= 10 {
		digits = string(rune('0'+i%10)) + digits
	}
	return digits
}

func TestRoundRobinStaggeredArrivals(t *testing.T) {
	procs := []Process{
		{"P1", 0, 5, 0},
		{"P2", 1, 4, 0},
		{"P3", 2, 2, 0},
		{"P4", 4, 1, 0},
	}
	r, err := RoundRobin(procs, 2, 0)
	if err != nil {
		t.Fatalf("RoundRobin: %v", err)
	}

	want := "P1[0,2) P2[2,4) P3[4,6) P1[6,8) P4[8,9) P2[9,11) P1[11,12)"
	if got := gantt(r); got != want {
		t.Errorf("timeline\n got %s\nwant %s", got, want)
	}

	cases := []struct {
		completion, turnaround, waiting, response, preemptions int
	}{
		{12, 12, 7, 0, 2}, // P1
		{11, 10, 6, 1, 1}, // P2
		{6, 4, 2, 2, 0},   // P3
		{9, 5, 4, 4, 0},   // P4
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

	if r.Makespan != 12 || r.BusyTime != 12 || r.IdleTime != 0 {
		t.Errorf("got makespan=%d busy=%d idle=%d, want 12/12/0", r.Makespan, r.BusyTime, r.IdleTime)
	}
	if avg := r.AvgWaiting(); avg != 4.75 {
		t.Errorf("average waiting time = %v, want 4.75", avg)
	}
}

// A process that finishes inside its slice must leave the queue at once, so
// the next process starts before the quantum would have expired.
func TestProcessLeavesQueueWhenItCompletesEarly(t *testing.T) {
	procs := []Process{{"P1", 0, 1, 0}, {"P2", 0, 4, 0}}
	r, err := RoundRobin(procs, 3, 0)
	if err != nil {
		t.Fatalf("RoundRobin: %v", err)
	}
	want := "P1[0,1) P2[1,4) P2[4,5)"
	if got := gantt(r); got != want {
		t.Errorf("timeline\n got %s\nwant %s", got, want)
	}
	if r.Stats[0].Preemptions != 0 {
		t.Errorf("P1 finished inside its slice but was preempted %d time(s)", r.Stats[0].Preemptions)
	}
}

func TestIdleCPUBetweenArrivals(t *testing.T) {
	procs := []Process{{"P1", 0, 2, 0}, {"P2", 5, 3, 0}}
	r, err := RoundRobin(procs, 2, 0)
	if err != nil {
		t.Fatalf("RoundRobin: %v", err)
	}
	want := "P1[0,2) idle[2,5) P2[5,7) P2[7,8)"
	if got := gantt(r); got != want {
		t.Errorf("timeline\n got %s\nwant %s", got, want)
	}
	if r.IdleTime != 3 || r.BusyTime != 5 || r.Makespan != 8 {
		t.Errorf("got idle=%d busy=%d makespan=%d, want 3/5/8", r.IdleTime, r.BusyTime, r.Makespan)
	}
	if u := r.Utilization(); u != 62.5 {
		t.Errorf("utilization = %v, want 62.5", u)
	}
}

// With a quantum at least as long as the longest burst, round robin degenerates
// into first-come first-served.
func TestLargeQuantumBehavesLikeFCFS(t *testing.T) {
	procs := []Process{{"P1", 0, 4, 0}, {"P2", 0, 3, 0}, {"P3", 0, 2, 0}}
	r, err := RoundRobin(procs, 10, 0)
	if err != nil {
		t.Fatalf("RoundRobin: %v", err)
	}
	want := "P1[0,4) P2[4,7) P3[7,9)"
	if got := gantt(r); got != want {
		t.Errorf("timeline\n got %s\nwant %s", got, want)
	}
	for _, s := range r.Stats {
		if s.Preemptions != 0 {
			t.Errorf("%s was preempted with a quantum larger than its burst", s.Name)
		}
	}
}

func TestContextSwitchOverhead(t *testing.T) {
	procs := []Process{{"P1", 0, 3, 0}, {"P2", 0, 3, 0}}
	r, err := RoundRobin(procs, 2, 1)
	if err != nil {
		t.Fatalf("RoundRobin: %v", err)
	}
	// Switches are charged only when the CPU goes to a different process.
	want := "P1[0,2) cs[2,3) P2[3,5) cs[5,6) P1[6,7) cs[7,8) P2[8,9)"
	if got := gantt(r); got != want {
		t.Errorf("timeline\n got %s\nwant %s", got, want)
	}
	if r.SwitchTime != 3 || r.Makespan != 9 {
		t.Errorf("got switchTime=%d makespan=%d, want 3/9", r.SwitchTime, r.Makespan)
	}
}

// Whatever the quantum, the CPU must run for exactly the sum of the bursts and
// every process must finish after it arrived.
func TestBusyTimeMatchesTotalBurstForEveryQuantum(t *testing.T) {
	procs := []Process{{"P1", 0, 7, 0}, {"P2", 3, 4, 0}, {"P3", 3, 1, 0}, {"P4", 20, 5, 0}}
	total := 0
	for _, p := range procs {
		total += p.Burst
	}
	for q := 1; q <= 10; q++ {
		r, err := RoundRobin(procs, q, 0)
		if err != nil {
			t.Fatalf("quantum %d: %v", q, err)
		}
		if r.BusyTime != total {
			t.Errorf("quantum %d: busy time = %d, want %d", q, r.BusyTime, total)
		}
		for _, s := range r.Stats {
			if s.Completion <= s.Arrival {
				t.Errorf("quantum %d: %s completes at %d but arrives at %d", q, s.Name, s.Completion, s.Arrival)
			}
			if s.Waiting < 0 || s.Response < 0 {
				t.Errorf("quantum %d: %s has negative waiting/response time", q, s.Name)
			}
		}
	}
}

func TestInvalidInput(t *testing.T) {
	cases := []struct {
		name          string
		procs         []Process
		quantum       int
		contextSwitch int
	}{
		{"zero quantum", []Process{{"P1", 0, 3, 0}}, 0, 0},
		{"negative quantum", []Process{{"P1", 0, 3, 0}}, -1, 0},
		{"negative context switch", []Process{{"P1", 0, 3, 0}}, 2, -1},
		{"no processes", nil, 2, 0},
		{"zero burst", []Process{{"P1", 0, 0, 0}}, 2, 0},
		{"negative arrival", []Process{{"P1", -1, 3, 0}}, 2, 0},
	}
	for _, tc := range cases {
		if _, err := RoundRobin(tc.procs, tc.quantum, tc.contextSwitch); err == nil {
			t.Errorf("%s: expected an error, got none", tc.name)
		}
	}
}

func TestParseList(t *testing.T) {
	procs, err := parseList("P1:0:5, P2:1:3")
	if err != nil {
		t.Fatalf("parseList: %v", err)
	}
	want := []Process{{"P1", 0, 5, 0}, {"P2", 1, 3, 0}}
	if len(procs) != len(want) {
		t.Fatalf("got %d processes, want %d", len(procs), len(want))
	}
	for i := range want {
		if procs[i] != want[i] {
			t.Errorf("process %d = %+v, want %+v", i, procs[i], want[i])
		}
	}

	// The fourth field is the optional priority.
	withPriority, err := parseList("P1:0:5:3")
	if err != nil {
		t.Fatalf("parseList with priority: %v", err)
	}
	if withPriority[0] != (Process{"P1", 0, 5, 3}) {
		t.Errorf("got %+v, want {P1 0 5 3}", withPriority[0])
	}

	for _, bad := range []string{"P1:0", "P1:x:5", "P1:0:5:x", "P1:0:5:9:1", ""} {
		if _, err := parseList(bad); err == nil {
			t.Errorf("parseList(%q): expected an error, got none", bad)
		}
	}
}
