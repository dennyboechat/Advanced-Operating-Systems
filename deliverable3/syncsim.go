package main

import (
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Every simulated process below is a goroutine. They share data the way OS
// processes share a memory segment or a file, and coordinate only through the
// Semaphore and Mutex types in semaphore.go.

// ---------------------------------------------------------------------------
// Race condition on shared memory

// RaceResult is the outcome of the shared-counter experiment.
type RaceResult struct {
	Synchronized bool
	Expected     int64 // processes x increments
	Got          int64 // final value of the shared counter
	Blocks       int   // times a process waited for the mutex
	Wall         time.Duration
}

// Lost is the number of increments that were overwritten by another process.
func (r RaceResult) Lost() int64 { return r.Expected - r.Got }

// SharedCounter runs procs processes that each add 1 to a shared counter iters
// times. An increment is a read-modify-write: read the counter, compute the
// new value, write it back. The process yields between the read and the write,
// the way a time slice can end there on a real CPU.
//
// Without synchronization two processes can read the same value and both
// write value+1, so one increment is lost. With synchronized set, the whole
// read-modify-write is a critical section guarded by a Mutex and no update is
// ever lost.
//
// The counter is read and written with atomic loads and stores so the program
// stays free of data races in Go's sense (and passes go test -race); the race
// being shown is the logical one between the separate read and write.
func SharedCounter(procs, iters int, synchronized bool) (RaceResult, error) {
	if procs < 1 || iters < 1 {
		return RaceResult{}, fmt.Errorf("need at least 1 process and 1 increment, got %d and %d", procs, iters)
	}
	var counter int64
	lock := NewMutex()

	start := time.Now()
	var wg sync.WaitGroup
	for p := 0; p < procs; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				if synchronized {
					lock.Lock()
				}
				v := atomic.LoadInt64(&counter)  // read
				runtime.Gosched()                // preempted between read and write
				atomic.StoreInt64(&counter, v+1) // write
				if synchronized {
					lock.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	return RaceResult{
		Synchronized: synchronized,
		Expected:     int64(procs) * int64(iters),
		Got:          atomic.LoadInt64(&counter),
		Blocks:       lock.Blocks(),
		Wall:         time.Since(start),
	}, nil
}

// ---------------------------------------------------------------------------
// Producer-consumer on a bounded buffer

// PCConfig configures a producer-consumer run.
type PCConfig struct {
	Producers int
	Consumers int
	Capacity  int           // slots in the bounded buffer
	Items     int           // items each producer makes
	Delay     time.Duration // upper bound of the random time to produce or consume an item
	Seed      int64
	Log       func(format string, args ...any) // per-event output, may be nil
}

// Item is one unit of work passed from a producer to a consumer.
type Item struct {
	Producer int
	Seq      int  // 1-based, per producer
	stop     bool // end-of-stream marker, one per consumer
}

func (it Item) String() string { return fmt.Sprintf("item %d.%d", it.Producer, it.Seq) }

// PCResult is the outcome of a producer-consumer run.
type PCResult struct {
	Produced     []int    // items made by each producer
	Consumed     []int    // items taken by each consumer
	Received     [][]Item // items each consumer took, in the order it took them
	MaxOccupancy int      // most items ever in the buffer at once
	Overflows    int      // insertions into a full buffer (must be 0)
	Underflows   int      // removals from an empty buffer (must be 0)
	FullWaits    int      // times a producer blocked on a full buffer
	EmptyWaits   int      // times a consumer blocked on an empty buffer
	Wall         time.Duration
}

// boundedBuffer is a circular buffer shared by producers and consumers. Three
// semaphores make it safe:
//
//	empty  counts free slots   - a producer waits on it, so it blocks when full
//	full   counts filled slots - a consumer waits on it, so it blocks when empty
//	mutex  guards the slots and indices, so one process touches them at a time
type boundedBuffer struct {
	slots   []Item
	in, out int
	count   int

	empty *Semaphore
	full  *Semaphore
	mutex *Mutex

	maxCount   int
	overflows  int
	underflows int
}

func newBoundedBuffer(capacity int) *boundedBuffer {
	return &boundedBuffer{
		slots: make([]Item, capacity),
		empty: NewSemaphore(capacity),
		full:  NewSemaphore(0),
		mutex: NewMutex(),
	}
}

// put inserts an item, blocking while the buffer is full. It returns the
// number of items in the buffer right after the insertion.
func (b *boundedBuffer) put(it Item) int {
	b.empty.Wait()
	b.mutex.Lock()
	if b.count == len(b.slots) {
		b.overflows++ // impossible while the semaphores are used correctly
	}
	b.slots[b.in] = it
	b.in = (b.in + 1) % len(b.slots)
	b.count++
	b.maxCount = max(b.maxCount, b.count)
	n := b.count
	b.mutex.Unlock()
	b.full.Signal()
	return n
}

// take removes the oldest item, blocking while the buffer is empty. It returns
// the item and the number of items left in the buffer.
func (b *boundedBuffer) take() (Item, int) {
	b.full.Wait()
	b.mutex.Lock()
	if b.count == 0 {
		b.underflows++ // impossible while the semaphores are used correctly
	}
	it := b.slots[b.out]
	b.slots[b.out] = Item{}
	b.out = (b.out + 1) % len(b.slots)
	b.count--
	n := b.count
	b.mutex.Unlock()
	b.empty.Signal()
	return it, n
}

// ProducerConsumer runs the bounded-buffer problem: producers make items and
// put them in the buffer, consumers take them out. A producer facing a full
// buffer and a consumer facing an empty one sleep on a semaphore instead of
// spinning. When every producer is done, one end-of-stream marker per consumer
// goes through the same buffer so each consumer knows when to stop.
func ProducerConsumer(cfg PCConfig) (PCResult, error) {
	if cfg.Producers < 1 || cfg.Consumers < 1 {
		return PCResult{}, fmt.Errorf("need at least 1 producer and 1 consumer")
	}
	if cfg.Capacity < 1 {
		return PCResult{}, fmt.Errorf("buffer capacity must be at least 1, got %d", cfg.Capacity)
	}
	if cfg.Items < 0 {
		return PCResult{}, fmt.Errorf("items per producer cannot be negative, got %d", cfg.Items)
	}
	logf := cfg.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}

	buf := newBoundedBuffer(cfg.Capacity)
	res := PCResult{
		Produced: make([]int, cfg.Producers),
		Consumed: make([]int, cfg.Consumers),
		Received: make([][]Item, cfg.Consumers),
	}

	start := time.Now()
	var producers, consumers sync.WaitGroup

	for c := 0; c < cfg.Consumers; c++ {
		consumers.Add(1)
		go func(c int) {
			defer consumers.Done()
			rng := rand.New(rand.NewSource(cfg.Seed + 1000 + int64(c)))
			for {
				it, left := buf.take()
				if it.stop {
					logf("Consumer %d got the end-of-stream marker, stops", c+1)
					return
				}
				res.Consumed[c]++
				res.Received[c] = append(res.Received[c], it)
				logf("Consumer %d took %s     buffer %d/%d", c+1, it, left, cfg.Capacity)
				pause(rng, cfg.Delay) // consume it
			}
		}(c)
	}

	for p := 0; p < cfg.Producers; p++ {
		producers.Add(1)
		go func(p int) {
			defer producers.Done()
			rng := rand.New(rand.NewSource(cfg.Seed + int64(p)))
			for i := 1; i <= cfg.Items; i++ {
				pause(rng, cfg.Delay) // produce it
				it := Item{Producer: p + 1, Seq: i}
				n := buf.put(it)
				res.Produced[p]++
				logf("Producer %d put  %s     buffer %d/%d", p+1, it, n, cfg.Capacity)
			}
			logf("Producer %d done after %d item(s)", p+1, cfg.Items)
		}(p)
	}

	producers.Wait()
	for c := 0; c < cfg.Consumers; c++ {
		buf.put(Item{stop: true})
	}
	consumers.Wait()

	res.MaxOccupancy = buf.maxCount
	res.Overflows = buf.overflows
	res.Underflows = buf.underflows
	res.FullWaits = buf.empty.Blocks()
	res.EmptyWaits = buf.full.Blocks()
	res.Wall = time.Since(start)
	return res, nil
}

// pause sleeps for a random duration in [0, limit).
func pause(rng *rand.Rand, limit time.Duration) {
	if limit > 0 {
		time.Sleep(time.Duration(rng.Int63n(int64(limit))))
	}
}

// ---------------------------------------------------------------------------
// Dining philosophers

// DiningStrategy is how a philosopher picks up the two forks.
type DiningStrategy int

const (
	// DiningOrdered numbers the forks and has every philosopher pick up the
	// lower-numbered one first. A cycle of waits would need someone to hold a
	// higher fork while waiting for a lower one, which nobody ever does, so
	// circular wait - and therefore deadlock - cannot happen.
	DiningOrdered DiningStrategy = iota
	// DiningWaiter seats at most N-1 philosophers at once (a counting
	// semaphore). With one seat empty, at least one seated philosopher can
	// always get both forks.
	DiningWaiter
	// DiningNaive picks up the left fork, then the right one. When everyone
	// is holding their left fork they all wait forever: deadlock. It is here
	// to show what the other two strategies prevent.
	DiningNaive
)

func (s DiningStrategy) String() string {
	switch s {
	case DiningWaiter:
		return "waiter"
	case DiningNaive:
		return "naive"
	default:
		return "ordered"
	}
}

// ParseDiningStrategy turns the -strategy flag value into a strategy.
func ParseDiningStrategy(s string) (DiningStrategy, error) {
	switch s {
	case "ordered", "order", "hierarchy":
		return DiningOrdered, nil
	case "waiter", "arbitrator":
		return DiningWaiter, nil
	case "naive":
		return DiningNaive, nil
	default:
		return 0, fmt.Errorf("unknown strategy %q (use \"ordered\", \"waiter\" or \"naive\")", s)
	}
}

// DiningConfig configures a dining philosophers run.
type DiningConfig struct {
	Philosophers int
	Meals        int           // meals each philosopher eats before leaving
	Think        time.Duration // upper bound of a random thinking time
	Eat          time.Duration // upper bound of a random eating time
	Reach        time.Duration // pause between picking up the first and the second fork
	Strategy     DiningStrategy
	Seed         int64
	Log          func(format string, args ...any)
}

// DiningResult is the outcome of a dining philosophers run.
type DiningResult struct {
	Meals              []int // meals eaten by each philosopher
	MaxEating          int   // most philosophers eating at the same time
	NeighborViolations int   // times two neighbours ate at once (must be 0)
	ForkWaits          int   // times a philosopher had to wait for a fork
	Deadlocked         bool
	Wall               time.Duration
}

// table holds the state the philosophers share besides the forks, so the run
// can check mutual exclusion and detect a deadlock.
type table struct {
	mu      sync.Mutex
	n       int
	holding []int  // forks each philosopher is holding
	waiting []bool // blocked on a fork
	eating  []bool

	eatingNow          int
	maxEating          int
	neighborViolations int
	deadlocked         bool
}

// wantFork records that philosopher i is about to block on a fork and reports
// whether that completes a deadlock: every philosopher holds one fork and
// waits for the next, a cycle in which nobody can ever release anything.
func (t *table) wantFork(i int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.waiting[i] = true
	for j := 0; j < t.n; j++ {
		if !t.waiting[j] || t.holding[j] != 1 {
			return false
		}
	}
	t.deadlocked = true
	return true
}

func (t *table) gotFork(i int) {
	t.mu.Lock()
	t.waiting[i] = false
	t.holding[i]++
	t.mu.Unlock()
}

func (t *table) startEating(i int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	left, right := (i+t.n-1)%t.n, (i+1)%t.n
	if t.eating[left] || t.eating[right] {
		t.neighborViolations++
	}
	t.eating[i] = true
	t.eatingNow++
	t.maxEating = max(t.maxEating, t.eatingNow)
}

func (t *table) stopEating(i int) {
	t.mu.Lock()
	t.eating[i] = false
	t.eatingNow--
	t.holding[i] = 0
	t.mu.Unlock()
}

// DiningPhilosophers seats the philosophers at a round table with one fork
// between each pair. Philosopher i needs fork i (left) and fork i+1 (right) to
// eat. Every philosopher starts hungry, so they all reach for forks at once,
// which is the worst case for deadlock.
//
// A naive run that deadlocks is detected the moment the last philosopher
// blocks; the forks are then closed so every philosopher gives up and the run
// returns with Deadlocked set.
func DiningPhilosophers(cfg DiningConfig) (DiningResult, error) {
	n := cfg.Philosophers
	if n < 2 {
		return DiningResult{}, fmt.Errorf("need at least 2 philosophers, got %d", n)
	}
	if cfg.Meals < 1 {
		return DiningResult{}, fmt.Errorf("each philosopher needs at least 1 meal, got %d", cfg.Meals)
	}
	logf := cfg.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}

	forks := make([]*Mutex, n)
	for i := range forks {
		forks[i] = NewMutex()
	}
	seats := NewSemaphore(n - 1) // the waiter
	t := &table{n: n, holding: make([]int, n), waiting: make([]bool, n), eating: make([]bool, n)}
	meals := make([]int, n)
	var forkWaits atomic.Int64

	var once sync.Once
	tearDown := func() {
		once.Do(func() {
			logf("DEADLOCK: every philosopher holds one fork and waits for the next - circular wait")
			for _, f := range forks {
				f.Close()
			}
			seats.Close()
		})
	}

	// pickUp acquires one fork, recording the wait so a deadlock can be spotted.
	pickUp := func(i, fork int, side string) bool {
		if forks[fork].TryLock() {
			t.gotFork(i)
			logf("Philosopher %d picks up fork %d (%s)", i, fork, side)
			return true
		}
		logf("Philosopher %d waits for fork %d (%s)", i, fork, side)
		forkWaits.Add(1)
		if t.wantFork(i) {
			tearDown()
			return false
		}
		if !forks[fork].Lock() {
			return false
		}
		t.gotFork(i)
		logf("Philosopher %d picks up fork %d (%s)", i, fork, side)
		return true
	}

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(cfg.Seed + int64(i)))
			left, right := i, (i+1)%n
			first, second := left, right
			firstSide, secondSide := "left", "right"
			if cfg.Strategy == DiningOrdered && right < left {
				first, second = right, left
				firstSide, secondSide = "right", "left"
			}

			for meal := 1; meal <= cfg.Meals; meal++ {
				logf("Philosopher %d is hungry", i)
				if cfg.Strategy == DiningWaiter {
					if !seats.Wait() {
						return
					}
				}
				if !pickUp(i, first, firstSide) {
					return
				}
				if cfg.Reach > 0 {
					time.Sleep(cfg.Reach)
				}
				if !pickUp(i, second, secondSide) {
					return
				}

				t.startEating(i)
				meals[i]++
				logf("Philosopher %d eats (meal %d of %d)", i, meal, cfg.Meals)
				pause(rng, cfg.Eat)
				t.stopEating(i)

				forks[second].Unlock()
				forks[first].Unlock()
				if cfg.Strategy == DiningWaiter {
					seats.Signal()
				}
				logf("Philosopher %d puts down forks %d and %d, thinks", i, first, second)
				pause(rng, cfg.Think)
			}
			logf("Philosopher %d is full and leaves the table", i)
		}(i)
	}
	wg.Wait()

	res := DiningResult{
		Meals:              meals,
		MaxEating:          t.maxEating,
		NeighborViolations: t.neighborViolations,
		ForkWaits:          int(forkWaits.Load()),
		Deadlocked:         t.deadlocked,
		Wall:               time.Since(start),
	}
	return res, nil
}
