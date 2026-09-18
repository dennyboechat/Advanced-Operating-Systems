package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// testTick is short enough to keep the tests quick, but long enough that
// scheduling jitter stays well below half a tick, which is what the run needs
// in order to round elapsed real time back to exact time units.
const testTick = 30 * time.Millisecond

func skipIfShort(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("live runs take real time")
	}
}

// A live round-robin run must produce exactly the timeline the instant
// simulation predicts, and take roughly as long as the makespan says.
func TestLiveRoundRobinMatchesSimulation(t *testing.T) {
	skipIfShort(t)

	procs := []Process{
		{"P1", 0, 3, 0},
		{"P2", 1, 2, 0},
		{"P3", 2, 3, 0},
	}
	want, err := RoundRobin(procs, 2, 0)
	if err != nil {
		t.Fatalf("RoundRobin: %v", err)
	}

	start := time.Now()
	got, err := RunLive(procs, LiveConfig{Policy: PolicyRoundRobin, Quantum: 2, Tick: testTick})
	if err != nil {
		t.Fatalf("RunLive: %v", err)
	}
	elapsed := time.Since(start)

	if gantt(got) != gantt(want) {
		t.Errorf("timeline\n got %s\nwant %s", gantt(got), gantt(want))
	}
	if got.Makespan != want.Makespan {
		t.Errorf("makespan = %d, want %d", got.Makespan, want.Makespan)
	}
	expected := time.Duration(want.Makespan) * testTick
	if elapsed < expected {
		t.Errorf("run took %v, cannot be less than %v of simulated work", elapsed, expected)
	}
	if elapsed > expected+time.Duration(len(want.Timeline)+4)*testTick {
		t.Errorf("run took %v, far longer than the expected %v", elapsed, expected)
	}
}

// Each slice must last the time slice it was granted: a preempted round-robin
// process runs for exactly one quantum of real time.
func TestLiveRoundRobinSlicesLastOneQuantum(t *testing.T) {
	skipIfShort(t)

	procs := []Process{{"P1", 0, 4, 0}, {"P2", 0, 4, 0}}
	r, err := RunLive(procs, LiveConfig{Policy: PolicyRoundRobin, Quantum: 2, Tick: testTick})
	if err != nil {
		t.Fatalf("RunLive: %v", err)
	}

	quantum := 2 * testTick
	for _, s := range r.Timeline {
		if s.Kind != KindRun {
			continue
		}
		if s.Duration() != 2 {
			t.Errorf("%s ran %d unit(s), want the whole quantum of 2", s.Name, s.Duration())
		}
		if off := s.Wall - quantum; off < -testTick/2 || off > testTick/2 {
			t.Errorf("%s ran for %v of real time, want about %v", s.Name, s.Wall, quantum)
		}
	}
}

// Under priority scheduling the running process must lose the CPU the moment a
// higher-priority process arrives, not at the end of its burst.
func TestLivePriorityPreemptsImmediately(t *testing.T) {
	skipIfShort(t)

	procs := []Process{
		{"P1", 0, 6, 3},
		{"P2", 2, 4, 1},
		{"P3", 3, 2, 0},
	}
	want, err := PriorityScheduling(procs, LowNumberFirst, 0)
	if err != nil {
		t.Fatalf("PriorityScheduling: %v", err)
	}

	got, err := RunLive(procs, LiveConfig{Policy: PolicyPriority, Rule: LowNumberFirst, Tick: testTick})
	if err != nil {
		t.Fatalf("RunLive: %v", err)
	}

	if gantt(got) != gantt(want) {
		t.Errorf("timeline\n got %s\nwant %s", gantt(got), gantt(want))
	}

	// P1 was granted its whole 6-unit burst but must have been stopped after
	// the 2 units it had run when P2 arrived.
	first := got.Timeline[0]
	if first.Name != "P1" || first.Duration() != 2 || first.Preemptor != "P2" {
		t.Fatalf("first slice = %+v, want P1 preempted by P2 after 2 units", first)
	}
	if off := first.Wall - 2*testTick; off < -testTick/2 || off > testTick/2 {
		t.Errorf("P1 held the CPU for %v of real time, want about %v", first.Wall, 2*testTick)
	}

	for i := range got.Stats {
		if got.Stats[i] != want.Stats[i] {
			t.Errorf("stats for %s\n got %+v\nwant %+v", got.Stats[i].Name, got.Stats[i], want.Stats[i])
		}
	}
}

func TestLiveIdleAndContextSwitch(t *testing.T) {
	skipIfShort(t)

	procs := []Process{{"P1", 0, 2, 1}, {"P2", 4, 2, 0}}
	want, err := PriorityScheduling(procs, LowNumberFirst, 1)
	if err != nil {
		t.Fatalf("PriorityScheduling: %v", err)
	}
	got, err := RunLive(procs, LiveConfig{
		Policy: PolicyPriority, Rule: LowNumberFirst, ContextSwitch: 1, Tick: testTick,
	})
	if err != nil {
		t.Fatalf("RunLive: %v", err)
	}
	if gantt(got) != gantt(want) {
		t.Errorf("timeline\n got %s\nwant %s", gantt(got), gantt(want))
	}
	if got.IdleTime != want.IdleTime || got.SwitchTime != want.SwitchTime {
		t.Errorf("got idle=%d switch=%d, want idle=%d switch=%d",
			got.IdleTime, got.SwitchTime, want.IdleTime, want.SwitchTime)
	}
}

// The event log must report what happened while it was happening.
func TestLiveLogReportsEvents(t *testing.T) {
	skipIfShort(t)

	var events []string
	var stamps []time.Duration
	start := time.Now()

	procs := []Process{{"P1", 0, 3, 2}, {"P2", 1, 1, 0}}
	_, err := RunLive(procs, LiveConfig{
		Policy: PolicyPriority, Rule: LowNumberFirst, Tick: testTick,
		Log: func(format string, args ...any) {
			events = append(events, fmt.Sprintf(format, args...))
			stamps = append(stamps, time.Since(start))
		},
	})
	if err != nil {
		t.Fatalf("RunLive: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("the log recorded nothing")
	}
	if last := stamps[len(stamps)-1]; last < 3*testTick {
		t.Errorf("the last event was logged after %v, so it cannot have been live", last)
	}
	preempted := false
	for _, e := range events {
		if strings.Contains(e, "P1 preempted by P2") {
			preempted = true
		}
	}
	if !preempted {
		t.Errorf("no preemption was logged; events: %v", events)
	}
}

func TestLiveInvalidInput(t *testing.T) {
	cases := []struct {
		name string
		cfg  LiveConfig
	}{
		{"zero tick", LiveConfig{Policy: PolicyRoundRobin, Quantum: 2}},
		{"negative tick", LiveConfig{Policy: PolicyRoundRobin, Quantum: 2, Tick: -time.Second}},
		{"zero quantum", LiveConfig{Policy: PolicyRoundRobin, Tick: time.Millisecond}},
		{"negative context switch", LiveConfig{
			Policy: PolicyPriority, Tick: time.Millisecond, ContextSwitch: -1,
		}},
	}
	for _, tc := range cases {
		if _, err := RunLive([]Process{{"P1", 0, 1, 0}}, tc.cfg); err == nil {
			t.Errorf("%s: expected an error, got none", tc.name)
		}
	}
	if _, err := RunLive(nil, LiveConfig{Policy: PolicyPriority, Tick: time.Millisecond}); err == nil {
		t.Error("no processes: expected an error, got none")
	}
}

func TestParsePolicy(t *testing.T) {
	for _, s := range []string{"rr", "round-robin", "roundrobin"} {
		if p, err := ParsePolicy(s); err != nil || p != PolicyRoundRobin {
			t.Errorf("ParsePolicy(%q) = %v, %v", s, p, err)
		}
	}
	for _, s := range []string{"priority", "prio"} {
		if p, err := ParsePolicy(s); err != nil || p != PolicyPriority {
			t.Errorf("ParsePolicy(%q) = %v, %v", s, p, err)
		}
	}
	if _, err := ParsePolicy("sjf"); err == nil {
		t.Error("ParsePolicy(\"sjf\"): expected an error, got none")
	}
}
