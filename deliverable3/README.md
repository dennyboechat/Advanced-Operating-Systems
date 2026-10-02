# Deliverable 3 — Memory Management and Process Synchronization (Go)

One program, `aos`, with three subcommands:

| Command | What it does |
|---------|--------------|
| `mem`   | **Paging.** Processes get pages, physical memory is a fixed number of fixed-size **page frames**, pages are loaded on demand, every **page fault** is counted, and when memory is full **FIFO** or **LRU** chooses the page to replace. Tracks memory used per process and shows memory overflow. |
| `sync`  | **Synchronization.** Counting **semaphores** and **mutexes** built on a condition variable, a **race condition** on shared memory with and without a mutex, **producer-consumer** on a bounded buffer, and the **dining philosophers** without deadlock. |
| `sched` | The CPU scheduler from Deliverable 2 (round robin, preemptive priority), unchanged. See `../deliverable2/README.md`. |

With no subcommand the arguments go to `sched`, so every Deliverable 2 command line
still works.

## Requirements

Go 1.24 or newer (the version in `go.mod`). No external dependencies.

## Build and run

```
go build -o aos .
./aos help
./aos mem -file workload.txt
./aos mem -algo both -frames 3 -refs "1 2 3 4 1 2 5 1 2 3 4 5" -sweep 5
./aos mem -i -algo lru -frames 3
./aos sync race
./aos sync pc -producers 2 -consumers 3 -buffer 4
./aos sync dining -strategy ordered
./aos sync dining -strategy naive
```

`go run . <command> ...` works too, without building.

---

## Part 1 — Memory management (`aos mem`)

### The model

- **Physical memory** is `-frames` page frames of `-pagesize` bytes each (default
  4 frames × 4 KB). Frames are shared by every process.
- **`alloc P1 4`** gives process `P1` a virtual address space of 4 pages and an empty
  **page table** (one entry per page: valid bit + frame number). Nothing is loaded yet.
  This is **demand paging**.
- **`access P1 2`** references page 2 of `P1`:
  - page table entry valid → **hit**; the frame's last-used time is updated;
  - not valid → **page fault**. The page is loaded into the lowest free frame, or,
    if every frame is taken, the **replacement algorithm** picks a victim frame. The
    victim's page table entry is invalidated, and its owner's eviction count goes up.
  - page outside the address space (`P1 9` when P1 has 4 pages) → **segmentation
    fault**. It is reported and counted, but nothing is loaded, and the run goes on.
- **`free P1`** deallocates the process: every frame it held becomes free, so the next
  faults use those frames instead of replacing someone's page.
- Replacement is **global**: a fault in one process can evict another process's page.

### Page replacement algorithms (`memory.go`)

Every reference advances a clock. Each frame records the clock value when its page
was **loaded** and when it was **last used**. When memory is full:

| `-algo` | Victim | Notes |
|---------|--------|-------|
| `fifo`  | the frame with the oldest **load** time, i.e. pages leave in the order they came in | Ignores how much a page is used. Suffers from **Belady's anomaly**: more frames can mean *more* faults. |
| `lru`   | the frame with the oldest **last-used** time, i.e. the page not referenced for the longest | A stack algorithm, so it never gets worse with more frames. |
| `both`  | runs the workload under each and prints a comparison | |

Clock values are unique, so there are no ties.

### Memory usage and overflow

For each process the report shows its virtual size, the pages **resident** right now
and the **memory used** (resident pages × page size), the **peak** memory it ever
held, its accesses, hits, **page faults**, fault rate, and how many of its pages were
evicted. The totals give page references, faults, hits, **replacements** (faults that
found memory full), and frames in use.

When the processes' address spaces add up to more pages than there are frames, the
trace says so: memory is **overcommitted** and from then on faults need page
replacement.

### Input

Operations come from `-file` (one per line, `#` comments allowed, `-file -` reads
stdin), from `-ops` (separated by `;`), from `-refs` (a reference string for a single
process `P1`, which is given just enough pages), or from the built-in sample.

| Operation | Meaning |
|-----------|---------|
| `alloc <pid> <pages>`    | create a process with that many virtual pages |
| `access <pid> <page>...` | reference pages in order; `access` can be left out: `P1 0 1 2` |
| `free <pid>`             | deallocate the process |
| `frames`                 | print the memory map |
| `stats`                  | print the per-process table |

| Flag        | Description                                             | Default |
|-------------|---------------------------------------------------------|---------|
| `-algo`     | `fifo`, `lru` or `both`                                 | `fifo`  |
| `-frames`   | number of page frames                                   | `4`     |
| `-pagesize` | page / frame size in bytes                              | `4096`  |
| `-file`     | workload file (`-` for stdin)                           | —       |
| `-ops`      | inline workload                                         | —       |
| `-refs`     | reference string for one process                        | —       |
| `-sweep N`  | also print FIFO and LRU faults for 1..N frames          | `0`     |
| `-i`        | interactive `mem>` prompt                               | `false` |
| `-quiet`    | summary only                                            | `false` |

### Example: a trace

```
$ ./aos mem -algo lru -frames 3 -ops "alloc A 3; alloc B 2; A 0 1; B 0; A 0; B 1; A 2 0; frames; free B; A 1 2"
Paging | LRU page replacement | 3 frame(s) x 4 KB = 12 KB of physical memory
Workload: 10 operation(s) (from -ops)

Trace
  alloc        A allocated 3 page(s), 12 KB of virtual memory, none resident yet
  alloc        B allocated 2 page(s), 8 KB of virtual memory, none resident yet
  overflow     5 page(s) allocated for 3 frame(s): memory is overcommitted, page replacement will be needed
  t=1    A:0     FAULT  loaded into free frame 0                    [A:0   -     -    ]
  t=2    A:1     FAULT  loaded into free frame 1                    [A:0   A:1   -    ]
  t=3    B:0     FAULT  loaded into free frame 2                    [A:0   A:1   B:0  ]
  t=4    A:0     hit    in frame 0                                  [A:0   A:1   B:0  ]
  t=5    B:1     FAULT  memory full, LRU replaces A:1 in frame 1    [A:0   B:1   B:0  ]
  t=6    A:2     FAULT  memory full, LRU replaces B:0 in frame 2    [A:0   B:1   A:2  ]
  t=7    A:0     hit    in frame 0                                  [A:0   B:1   A:2  ]

Memory map (3 of 3 frame(s) in use, 12 KB of 12 KB)
Frame   Page      Loaded   Last used
    0   A:0          t=1         t=7
    1   B:1          t=5         t=5
    2   A:2          t=6         t=6

  free         B freed, 1 frame(s) released, 2 of 3 now in use
  t=8    A:1     FAULT  loaded into free frame 1                    [A:0   A:1   A:2  ]
  t=9    A:2     hit    in frame 2                                  [A:0   A:1   A:2  ]

Process   Pages   Virtual   Resident   Memory used   Peak used   Accesses   Hits   Faults   Fault rate   Evicted
A             3     12 KB          3         12 KB       12 KB          7      3        4       57.14%         1
B             2      8 KB          0         freed        8 KB          2      0        2      100.00%         1

Page references:  9
Page faults:      6 (66.67%)
Hits:             3 (33.33%)
Replacements:     2 (faults that found memory full)
Frames in use:    3 of 3 now, peak 3 (12 KB of 12 KB)
```

At t=5 FIFO would have replaced `A:0`, the first page loaded. LRU replaces `A:1`
because `A:0` was used again at t=4, and `A:0` is then a hit at t=7.

### Example: FIFO vs LRU, and Belady's anomaly

```
$ ./aos mem -algo both -frames 3 -refs "1 2 3 4 1 2 5 1 2 3 4 5" -quiet -sweep 5
...
Comparison
Algorithm   Faults   Fault rate   Hits   Replacements
FIFO             9       75.00%      3              6
LRU             10       83.33%      2              7

Page faults by number of frames
Frames   FIFO    LRU
     1     12     12
     2     12     12
     3      9     10
     4     10      8   <- Belady's anomaly: FIFO faults more with more frames
     5      5      5
```

On the textbook string `7 0 1 2 0 3 0 4 2 3 0 3 2 1 2 0 1 7 0 1` with 3 frames, FIFO
gives 15 faults and LRU 12, the figures from *Operating System Concepts*.

### Interactive mode

`-i` turns the simulator into a small shell: every operation runs as soon as it is
typed, mistakes are reported without leaving, and `quit` prints the summary.

```
$ ./aos mem -i -algo lru -frames 2
Paging | LRU page replacement | 2 frame(s) x 4 KB = 8 KB of physical memory
Type "help" for the operations, "quit" to leave.
mem> alloc P1 3
  alloc        P1 allocated 3 page(s), 12 KB of virtual memory, none resident yet
  overflow     3 page(s) allocated for 2 frame(s): memory is overcommitted, page replacement will be needed
mem> P1 0 1 0 2
  t=1    P1:0    FAULT  loaded into free frame 0                    [P1:0  -    ]
  t=2    P1:1    FAULT  loaded into free frame 1                    [P1:0  P1:1 ]
  t=3    P1:0    hit    in frame 0                                  [P1:0  P1:1 ]
  t=4    P1:2    FAULT  memory full, LRU replaces P1:1 in frame 1   [P1:0  P1:2 ]
mem> P1 7
  SEGFAULT     segmentation fault: process P1 has pages 0-2, page 7 is outside its address space
mem> quit
```

`workload.txt` is a longer sample that covers allocation, overflow, replacement, a
segmentation fault, `free`, `frames` and `stats`.

---

## Part 2 — Process synchronization (`aos sync`)

Every simulated process is a goroutine. They share data the way OS processes share a
memory segment or a file, and coordinate **only** through the primitives in
`semaphore.go`.

### Semaphores and mutexes (`semaphore.go`)

- **`Semaphore`**: a counting semaphore. `Wait` (P) takes a unit and **blocks while the
  counter is zero**; `Signal` (V) gives one back and wakes a waiter. It is built on
  `sync.Cond`, so blocking is explicit: a waiter sleeps in `cond.Wait` and re-checks
  the counter when it wakes. It also counts how often callers had to block, and has
  `TryWait` and `Close` (wakes every waiter so a deadlocked group can be torn down).
- **`Mutex`**: a binary semaphore (initial value 1) for mutual exclusion. Unlocking a
  mutex that is not held panics.

### Race condition (`aos sync race`)

N processes each increment a shared counter K times. An increment is a
read-modify-write, and the process yields between the read and the write, as a time
slice could end there. Without a lock two processes read the same value and one
update is lost. With the read-modify-write in a mutex-guarded **critical section**,
nothing is lost.

```
$ ./aos sync race
Race condition on shared memory | 4 processes x 1000 increments of one shared counter

Mode             Expected      Final       Lost   Mutex waits   Time
no lock              4000       1000       3000             0   1ms
mutex                4000       4000          0            14   1ms
```

The counter is read and written with atomic loads and stores, so the program has no
data race in Go's sense and passes `go test -race`. The race it shows is the logical
one between the separate read and write, which is the bug a mutex fixes.

Flags: `-procs` (4), `-iters` (1000).

### Producer-consumer (`aos sync pc`)

Producers put items into a **bounded circular buffer**, and consumers take them out.
There are three synchronization objects:

| Object  | Initial  | Who waits on it | Effect |
|---------|----------|-----------------|--------|
| `empty` | capacity | producers       | a producer **blocks when the buffer is full** |
| `full`  | 0        | consumers       | a consumer **blocks when the buffer is empty** |
| `mutex` | 1        | both            | only one process touches the slots and indices at a time |

```
producer: wait(empty); lock(mutex); insert; unlock(mutex); signal(full)
consumer: wait(full);  lock(mutex); remove; unlock(mutex); signal(empty)
```

When every producer is done, one end-of-stream marker per consumer goes through the
same buffer, so each consumer knows when to stop. The run checks its own correctness:
every item is consumed exactly once (no item missing or duplicated), occupancy never
exceeds capacity, and there are no overflows or underflows.

```
$ ./aos sync pc -producers 2 -consumers 1 -buffer 2 -items 3 -delay 30ms
Producer-consumer | 2 producer(s), 1 consumer(s), buffer of 2, 3 item(s) per producer

  [   0.02s] Producer 2 put  item 2.1     buffer 1/2
  [   0.02s] Consumer 1 took item 2.1     buffer 0/2
  [   0.03s] Producer 2 put  item 2.2     buffer 1/2
  [   0.03s] Producer 1 put  item 1.1     buffer 2/2
  [   0.05s] Consumer 1 took item 2.2     buffer 1/2
  [   0.05s] Producer 1 put  item 1.2     buffer 2/2
  ...
  [   0.11s] Consumer 1 got the end-of-stream marker, stops

Producer 1   produced 3
Producer 2   produced 3
Consumer 1   consumed 6

Items produced / consumed:   6 / 6 (missing 0, duplicated 0)
Peak buffer occupancy:       2 of 2
Producer waits (full):       4
Consumer waits (empty):      1
Overflows / underflows:      0 / 0
```

Flags: `-producers` (2), `-consumers` (2), `-buffer` (3), `-items` per producer (5),
`-delay` maximum random time to produce or consume an item (50ms), `-seed`, `-quiet`.

### Dining philosophers (`aos sync dining`)

N philosophers sit at a round table with one fork (a `Mutex`) between each pair.
Philosopher *i* needs fork *i* (left) and fork *i+1* (right) to eat. Everyone starts
hungry, so they all reach for forks at once, the worst case for deadlock.

| `-strategy` | How deadlock is avoided |
|-------------|-------------------------|
| `ordered` (default) | **Resource ordering.** Every philosopher picks up the lower-numbered fork first. The last philosopher therefore reaches for fork 0 before fork N-1, which breaks the cycle. Circular wait is impossible, so deadlock is impossible. |
| `waiter`    | **Arbitrator.** A counting semaphore of N-1 seats: at most N-1 philosophers try to eat at once, so at least one of them can always get both forks. |
| `naive`     | **Not avoided.** Left fork, then right. Included to show the deadlock the other two prevent. |

A deadlock is **detected**, not waited out. The moment a philosopher is about to block
while every philosopher holds exactly one fork and is waiting for the next, the wait
cycle is complete and nobody can ever release a fork. The run reports it, closes the
forks so every philosopher gives up, and returns.

```
$ ./aos sync dining -strategy naive
Dining philosophers | 5 philosophers, 3 meal(s) each, strategy naive

  [   0.00s] Philosopher 4 is hungry
  [   0.00s] Philosopher 4 picks up fork 4 (left)
  ...
  [   0.00s] Philosopher 3 picks up fork 3 (left)
  [   0.01s] Philosopher 3 waits for fork 4 (right)
  [   0.01s] Philosopher 4 waits for fork 0 (right)
  [   0.01s] Philosopher 1 waits for fork 2 (right)
  [   0.01s] Philosopher 0 waits for fork 1 (right)
  [   0.01s] Philosopher 2 waits for fork 3 (right)
  [   0.01s] DEADLOCK: every philosopher holds one fork and waits for the next - circular wait
...
Result:                      DEADLOCK (every philosopher held one fork and waited for another)
```

```
$ ./aos sync dining -strategy ordered -quiet
Dining philosophers | 5 philosophers, 3 meal(s) each, strategy ordered

Philosopher 0   ate 3 of 3 meal(s)
...
Philosopher 4   ate 3 of 3 meal(s)

Result:                      every philosopher finished, no deadlock
Most eating at once:         2 (at most 2 possible)
Neighbours eating together:  0 (must be 0)
```

The run also checks mutual exclusion: two neighbours never eat at the same time, and
at most ⌊N/2⌋ philosophers eat at once.

Flags: `-strategy`, `-n` philosophers (5), `-meals` each (3), `-think` and `-eat`
maximum random durations (50ms), `-reach` pause between the first and second fork
(10ms; it makes the naive deadlock reliable), `-seed`, `-quiet`.

---

## Tests

```
go test ./...          # everything, including the Deliverable 2 scheduler tests
go test -short ./...   # skips the real-time scheduler tests
go test -race ./...    # the sync code is concurrent; it is race-detector clean
```

- **Paging**: textbook fault counts for FIFO (15) and LRU (12); Belady's anomaly
  (FIFO 9 → 10 faults from 3 to 4 frames) and that LRU never gets worse with more
  frames; FIFO and LRU choosing different victims; free frames used before any
  replacement; global replacement across processes; `free` releasing frames; usage
  counters (resident, peak, evicted, hits + faults = accesses); segmentation faults;
  invalid operations; workload parsing; the interactive prompt.
- **Synchronization**: the semaphore bounds concurrency, blocks until signalled,
  `TryWait` and `Close`; unlocking an unlocked mutex panics; the shared counter is
  exact with a mutex and loses updates without one; producer-consumer delivers every
  item exactly once over several configurations, keeps per-producer FIFO order and
  blocks producers on a full buffer; dining philosophers finish without deadlock under
  `ordered` and `waiter` for 2, 5 and 7 philosophers with no neighbour violations, and
  `naive` deadlocks and is detected.

## Design notes

- Like Deliverable 2 this is a **simulation**: processes are simulated, and in `sync`
  they are goroutines. Nothing is forked, and `mem` manages a model of memory, not real
  RAM.
- Paging is deterministic: the same workload always gives the same trace. The `sync`
  experiments are truly concurrent, so the order of events varies between runs (use
  `-seed` to fix the random delays). The invariants they check hold on every run.
- Victim selection scans the frames, O(frames) per fault. That is plenty for a
  simulator and keeps FIFO and LRU side by side in one function.

## Files

| File               | Purpose |
|--------------------|---------|
| `main.go`          | Subcommand dispatch and top-level help |
| `memory.go`        | Frames, page tables, demand paging, FIFO / LRU replacement, usage counters |
| `memory_cmd.go`    | `mem` CLI: workload parsing, trace, memory map, report, sweep, interactive prompt |
| `semaphore.go`     | Counting semaphore and mutex |
| `syncsim.go`       | Race condition, bounded-buffer producer-consumer, dining philosophers |
| `sync_cmd.go`      | `sync` CLI |
| `memory_test.go`   | Paging tests |
| `sync_test.go`     | Synchronization tests |
| `workload.txt`     | Sample paging workload |
| `sched_cmd.go`, `scheduler.go`, `priority.go`, `live.go`, `*_test.go`, `processes.txt` | Deliverable 2 scheduler |
