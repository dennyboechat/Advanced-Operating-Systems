# Deliverable 2 — CPU Scheduling (Go)

A CPU scheduler simulator with two algorithms and two execution engines.

**Algorithms**

- **Round robin** — every process is given a **time slice (quantum)** chosen by the
  user at run time. When the slice expires the scheduler switches to the next
  process in the ready queue; a process that finishes **before** its slice is over
  is removed from the queue immediately and the next process is scheduled right away.
- **Priority (preemptive)** — each process is assigned a **priority** and the most
  urgent ready process always holds the CPU. Processes of equal priority are served
  **first come, first served**. If a higher-priority process arrives while a
  lower-priority one is running, the running process is **preempted** at that instant
  and goes back into the queue with its remaining time. The ready queue is a
  **binary heap** (`container/heap`), so selecting the next process is O(log n).

**Engines**

- **Instant** (default) — a discrete-time simulation; the whole run is computed and
  printed at once.
- **Live** (`-live`) — the run actually takes real time. Each process is a
  **goroutine** that sleeps for the time it is granted, and **timers** drive every
  switch. Both engines produce the same timeline for the same input.

## Requirements

Go 1.24 or newer (the version declared in `go.mod`; the code itself uses nothing
newer than Go 1.21). No external dependencies.

## Run

```
go run . -algo rr -q 3 -file processes.txt
go run . -algo priority -file processes.txt
go run . -algo priority -live -tick 200ms -file processes.txt
go run . -h
```

Or build first:

```
go build -o sched .
./sched -algo priority -file processes.txt
```

| Flag        | Description                                                       | Default |
|-------------|-------------------------------------------------------------------|---------|
| `-algo`     | `rr` (round robin) or `priority` (preemptive priority)              | `rr`    |
| `-quantum`  | Round robin: time slice in time units (must be ≥ 1)                 | `2`     |
| `-q`        | Shorthand for `-quantum`                                            | `2`     |
| `-priority` | Priority: which number wins, `low` or `high`                        | `low`   |
| `-file`     | Process file, one `name arrival burst [priority]` per line          | —       |
| `-procs`    | Inline processes, e.g. `"P1:0:5:2,P2:1:3:0"`                        | —       |
| `-cs`       | Context-switch overhead in time units                               | `0`     |
| `-live`     | Execute the run in real time with timers instead of simulating it   | `false` |
| `-tick`     | Live: real duration of one time unit                                | `100ms` |
| `-quiet`    | Print only the Gantt chart and the summary table                    | `false` |

With neither `-file` nor `-procs`, a built-in sample process set is used.

`-priority low` (the default) is the usual Unix convention: **priority 0 outranks
priority 1**. Use `-priority high` when the larger number should be the more urgent
process.

## Input

### Process file (`-file`)

One process per line: `name arrival burst [priority]`. Blank lines and lines
starting with `#` are ignored; fields may be separated by spaces, tabs, commas or
semicolons. The priority is optional and defaults to 0, so a round-robin input file
needs only three columns.

```
# name  arrival  burst  priority
P1  0  5  2
P2  1  4  1
P3  2  7  3
P4  4  3  0
P5  9  2  1
```

### Inline (`-procs`)

The same records as `name:arrival:burst[:priority]`, comma separated:

```
-procs "P1:0:5:2,P2:1:3:0,P3:2:8:1"
```

Arrival times must be ≥ 0 and burst times ≥ 1; anything else is rejected with an
error naming the process.

## How it works

Both algorithms share the same model: processes are admitted to the ready queue when
their arrival time is reached, the CPU idles when the queue is empty but processes
are still to come, and `-cs` charges a context switch only when the CPU is handed to
a *different* process — a process re-dispatched to itself pays nothing.

### Round robin (`scheduler.go`)

1. The ready queue is FIFO.
2. The process at the front runs for `min(quantum, remaining)` time units.
3. Processes that arrive **while** a process is running are admitted before the
   preempted process goes to the back of the queue — the usual convention.
4. Quantum expired and time left → back of the queue. Remaining time reached zero →
   **removed from the queue**, and the next process is dispatched at that same
   instant instead of waiting out the slice.

With a quantum at least as long as the longest burst, round robin degenerates into
first-come first-served, which is one of the tests.

### Priority, preemptive (`priority.go`)

1. The ready queue is a binary heap ordered by *(priority, arrival time, submission
   order)*, so the most urgent process is at the root and equal priorities come out
   in FCFS order.
2. The dispatched process runs until it finishes, **unless** a process with a
   strictly higher priority arrives first — nothing already waiting can preempt it,
   since it was the most urgent process when it was dispatched.
3. On preemption the slice is cut at the newcomer's arrival time, the running
   process is pushed back into the heap with its remaining time, and the newcomer is
   dispatched. The trace names the process that caused each preemption.
4. An arrival of *equal* priority never preempts; it waits its turn, which is what
   keeps equal priorities first come, first served.

Preemptive priority scheduling can **starve** a low-priority process: in the sample
run below, `P3` (priority 3) only gets the CPU once everything else is done. There is
no aging.

### Live execution (`live.go`)

With `-live` the run is not computed, it is *performed*: one time unit lasts `-tick`
of real time, so `-tick 200ms` and a burst of 3 means the process really occupies the
CPU for 600 ms.

- Every process is a **goroutine** that waits for the scheduler to grant it a slice,
  then blocks in a `select` on two channels: a `time.Timer` for the granted duration,
  and a `stop` channel the scheduler uses to take the CPU back. Whichever fires first
  ends the slice — that `select` is the switch point.
- **Round robin** grants `min(quantum, remaining) × tick`, so a process is switched
  out exactly when its **time-slice timer** fires.
- **Priority** grants the whole remaining burst, but every arrival has its own timer;
  when one fires and the newcomer outranks the running process, the scheduler signals
  `stop` and the running process gives up the CPU **immediately**, mid-slice, instead
  of at the end of its burst.
- Arrival timers are scheduled against the start of the run, so arrival times cannot
  drift even if a slice overruns by a millisecond.
- The report is still in whole time units: elapsed real time is rounded to the
  nearest tick. A live run therefore produces **exactly the same timeline** as the
  instant simulation of the same input, which is what the live tests assert.

Pick a `-tick` comfortably larger than scheduling jitter (the default 100 ms is
safe; below ~10 ms the rounding back to time units can start to slip). Total real
time is roughly *makespan × tick*, so a 21-unit run at the default tick takes about
two seconds.

## Output

Every run prints the process table, an event trace, a Gantt chart and a summary
table. `-quiet` keeps only the chart and the summary.

### Round robin, quantum 3

```
$ go run . -algo rr -q 3 -file processes.txt

Round-robin scheduling | time slice = 3
Processes: 5 (from processes.txt)

Process   Arrival   Burst
P1              0       5
P2              1       4
P3              2       7
P4              4       3
P5              9       2

Execution trace
  t=0    P1   runs 3 unit(s) -> t=3, slice expired, 2 unit(s) left, requeued
  t=3    P2   runs 3 unit(s) -> t=6, slice expired, 1 unit(s) left, requeued
  t=6    P3   runs 3 unit(s) -> t=9, slice expired, 4 unit(s) left, requeued
  t=9    P1   runs 2 unit(s) -> t=11, finished, removed from the queue
  t=11   P4   runs 3 unit(s) -> t=14, finished, removed from the queue
  t=14   P2   runs 1 unit(s) -> t=15, finished, removed from the queue
  t=15   P5   runs 2 unit(s) -> t=17, finished, removed from the queue
  t=17   P3   runs 3 unit(s) -> t=20, slice expired, 1 unit(s) left, requeued
  t=20   P3   runs 1 unit(s) -> t=21, finished, removed from the queue

Gantt chart
  | P1 | P2 | P3 | P1 | P4 | P2 | P5 | P3 | P3 |
  0    3    6    9    11   14   15   17   20   21

Process   Arrival   Burst   Completion   Turnaround   Waiting   Response   Preemptions
P1              0       5           11           11         6          0             1
P2              1       4           15           14        10          2             1
P3              2       7           21           19        12          4             2
P4              4       3           14           10         7          7             0
P5              9       2           17            8         6          6             0

Average turnaround time: 12.40
Average waiting time:    8.20
Average response time:   3.80
Total time:              21 (busy 21, idle 0, context switches 0)
CPU utilization:         100.00%
Throughput:              0.2381 process/time unit
```

### Preemptive priority

Same process set, scheduled by priority. `P1` is preempted the moment the more
urgent `P2` arrives, `P2` the moment `P4` arrives, and `P3` (the least urgent) waits
until everything else is done.

```
$ go run . -algo priority -file processes.txt

Priority scheduling (preemptive) | lower number = higher priority, ties served FCFS
Processes: 5 (from processes.txt)

Process   Arrival   Burst   Priority
P1              0       5          2
P2              1       4          1
P3              2       7          3
P4              4       3          0
P5              9       2          1

Execution trace
  t=0    P1   runs 1 unit(s) -> t=1, preempted by P2, 4 unit(s) left, requeued
  t=1    P2   runs 3 unit(s) -> t=4, preempted by P4, 1 unit(s) left, requeued
  t=4    P4   runs 3 unit(s) -> t=7, finished, removed from the queue
  t=7    P2   runs 1 unit(s) -> t=8, finished, removed from the queue
  t=8    P1   runs 1 unit(s) -> t=9, preempted by P5, 3 unit(s) left, requeued
  t=9    P5   runs 2 unit(s) -> t=11, finished, removed from the queue
  t=11   P1   runs 3 unit(s) -> t=14, finished, removed from the queue
  t=14   P3   runs 7 unit(s) -> t=21, finished, removed from the queue

Gantt chart
  | P1 | P2 | P4 | P2 | P1 | P5 | P1 | P3 |
  0    1    4    7    8    9    11   14   21

Process   Arrival   Burst   Priority   Completion   Turnaround   Waiting   Response   Preemptions
P1              0       5          2           14           14         9          0             2
P2              1       4          1            8            7         3          0             1
P3              2       7          3           21           19        12         12             0
P4              4       3          0            7            3         0          0             0
P5              9       2          1           11            2         0          0             0

Average turnaround time: 9.00
Average waiting time:    4.80
Average response time:   2.40
Total time:              21 (busy 21, idle 0, context switches 0)
CPU utilization:         100.00%
Throughput:              0.2381 process/time unit
```

### Live round robin

In live mode the trace is replaced by an event log stamped with the real time
elapsed, printed as the run happens. Note the switch at 0.20 s, one quantum
(2 × 100 ms) after `P1` was dispatched.

```
$ go run . -algo rr -q 2 -live -tick 100ms -procs "P1:0:3,P2:1:2,P3:2:3"

Live execution
  [   0.00s] P1 arrives (burst 3, priority 0), added to the ready queue
  [   0.00s] P1 dispatched, runs for up to 2 unit(s) (200ms), 3 unit(s) of work left
  [   0.10s] P2 arrives (burst 2, priority 0), added to the ready queue
  [   0.20s] P3 arrives (burst 3, priority 0), added to the ready queue
  [   0.20s] P1 used its whole time slice of 2 unit(s) (201ms), 1 unit(s) left, requeued
  [   0.20s] P2 dispatched, runs for up to 2 unit(s) (200ms), 2 unit(s) of work left
  [   0.40s] P2 finished after 2 unit(s) (201ms), removed from the queue
  [   0.40s] P3 dispatched, runs for up to 2 unit(s) (200ms), 3 unit(s) of work left
  [   0.60s] P3 used its whole time slice of 2 unit(s) (201ms), 1 unit(s) left, requeued
  [   0.60s] P1 dispatched, runs for up to 1 unit(s) (100ms), 1 unit(s) of work left
  [   0.70s] P1 finished after 1 unit(s) (101ms), removed from the queue
  [   0.71s] P3 dispatched, runs for up to 1 unit(s) (100ms), 1 unit(s) of work left
  [   0.81s] P3 finished after 1 unit(s) (101ms), removed from the queue
```

### Live priority

Here `P1` is granted its whole 6-unit burst (600 ms) and loses the CPU 200 ms in, the
instant `P2` arrives — the switch is immediate, not at the end of the burst.

```
$ go run . -algo priority -live -tick 100ms -procs "P1:0:6:3,P2:2:4:1,P3:3:2:0"

Live execution
  [   0.00s] P1 arrives (burst 6, priority 3), added to the ready queue
  [   0.00s] P1 dispatched, runs for up to 6 unit(s) (600ms), 6 unit(s) of work left
  [   0.20s] P2 arrives (burst 4, priority 1), added to the ready queue
  [   0.20s] P1 preempted by P2 after 2 unit(s) (201ms), 4 unit(s) left, requeued
  [   0.20s] P2 dispatched, runs for up to 4 unit(s) (400ms), 4 unit(s) of work left
  [   0.30s] P3 arrives (burst 2, priority 0), added to the ready queue
  [   0.30s] P2 preempted by P3 after 1 unit(s) (100ms), 3 unit(s) left, requeued
  [   0.30s] P3 dispatched, runs for up to 2 unit(s) (200ms), 2 unit(s) of work left
  [   0.50s] P3 finished after 2 unit(s) (201ms), removed from the queue
  ...
```

A live run adds one line to the summary (this one from the run above, whose
makespan is 12 units):

```
Real time elapsed:       1.204s (1 time unit = 100ms)
```

### What the numbers mean

| Column / line     | Meaning                                                      |
|-------------------|--------------------------------------------------------------|
| Completion        | Time unit at which the process finished                       |
| Turnaround        | Completion − arrival                                          |
| Waiting           | Turnaround − burst                                            |
| Response          | First time on the CPU − arrival                               |
| Preemptions       | Times the process lost the CPU before it was done             |
| Total time        | Makespan, split into busy / idle / context-switch units       |
| CPU utilization   | Busy ÷ makespan                                               |
| Throughput        | Processes completed per time unit                             |

In the Gantt chart, `idle` marks an empty ready queue and `cs` a context switch.

## Tests

```
go test ./...          # everything, live tests included (a few seconds)
go test -short ./...   # skips the live tests, which take real time
go test -race ./...    # the live engine is concurrent
```

- **Round robin** — staggered arrivals against a hand-traced timeline, early
  completion inside a slice, an idle CPU between arrivals, a quantum larger than
  every burst (degenerates to FCFS), context-switch accounting, invariants across
  quanta 1–10, input parsing and validation.
- **Priority** — preemption by a higher-priority arrival, FCFS among equal
  priorities, selection order among waiting processes, the `high`-number rule, idle
  CPU, context-switch accounting, invariants under both rules, input validation, and
  the heap ordering itself (including `Peek` and the arrival/submission tie-breaks).
- **Live** — the timeline of a live run matches the instant simulation of the same
  input, round-robin slices last exactly one quantum of real time, priority
  preemption happens mid-slice at the arrival instant, idle and context-switch
  timing, and the event log is produced while the run is going on. These use a 30 ms
  tick; `go test -short` skips them.

## Design notes

- This is a **simulation**: "processes" are simulated jobs (goroutines in live mode),
  not real OS processes. Nothing is forked and no real CPU work is done — a process
  "running" means the scheduler has granted it the CPU for a span of simulated time.
- Time is **discrete**: arrivals, bursts and quanta are whole time units. In live
  mode one unit maps to `-tick` of real time.
- Ties are resolved deterministically everywhere: equal priorities by arrival time,
  equal arrival times by the order the processes were listed. The same input always
  produces the same timeline.

## Files

| File                | Purpose                                            |
|---------------------|-----------------------------------------------------|
| `scheduler.go`      | Shared types, validation, round-robin algorithm     |
| `priority.go`       | Heap-based ready queue, preemptive priority         |
| `live.go`           | Real-time execution: process goroutines, timers     |
| `main.go`           | CLI, input parsing, trace / Gantt / report          |
| `scheduler_test.go` | Round-robin and parsing tests                       |
| `priority_test.go`  | Priority-scheduling and heap tests                  |
| `live_test.go`      | Real-time execution tests                           |
| `processes.txt`     | Sample process set                                  |
| `go.mod`            | Module definition                                   |
