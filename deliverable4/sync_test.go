package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSemaphoreLimitsConcurrency(t *testing.T) {
	const limit = 3
	sem := NewSemaphore(limit)
	var inside, peak atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem.Wait()
			n := inside.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			inside.Add(-1)
			sem.Signal()
		}()
	}
	wg.Wait()
	if peak.Load() > limit {
		t.Errorf("%d goroutines inside a semaphore of %d", peak.Load(), limit)
	}
	if sem.Value() != limit {
		t.Errorf("counter is %d after every Wait was matched by a Signal, want %d", sem.Value(), limit)
	}
}

func TestSemaphoreBlocksUntilSignalled(t *testing.T) {
	sem := NewSemaphore(0)
	done := make(chan struct{})
	go func() {
		sem.Wait()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("Wait returned on a zero semaphore")
	case <-time.After(20 * time.Millisecond):
	}
	sem.Signal()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Signal did not wake the waiter")
	}
	if sem.Blocks() != 1 {
		t.Errorf("Blocks() = %d, want 1", sem.Blocks())
	}
}

func TestSemaphoreTryWaitAndClose(t *testing.T) {
	sem := NewSemaphore(1)
	if !sem.TryWait() || sem.TryWait() {
		t.Error("TryWait should succeed once on a semaphore of 1, then fail")
	}
	result := make(chan bool)
	go func() { result <- sem.Wait() }()
	time.Sleep(10 * time.Millisecond)
	sem.Close()
	if <-result {
		t.Error("Wait on a closed semaphore should return false")
	}
}

func TestMutexUnlockOfUnlockedPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("unlocking an unlocked mutex should panic")
		}
	}()
	NewMutex().Unlock()
}

func TestSharedCounterWithMutexLosesNothing(t *testing.T) {
	r, err := SharedCounter(8, 500, true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Got != r.Expected {
		t.Errorf("with a mutex the counter is %d, want %d", r.Got, r.Expected)
	}
}

func TestSharedCounterWithoutLockLosesUpdates(t *testing.T) {
	// Lost updates are a race, so they are likely but not certain on any one
	// run; a few attempts make the test reliable.
	for attempt := 0; attempt < 5; attempt++ {
		r, err := SharedCounter(8, 500, false)
		if err != nil {
			t.Fatal(err)
		}
		if r.Got > r.Expected {
			t.Fatalf("counter %d exceeds the %d increments made", r.Got, r.Expected)
		}
		if r.Lost() > 0 {
			return
		}
	}
	t.Error("no update was ever lost without a lock; the race is not being exercised")
}

func TestProducerConsumerDeliversEveryItemOnce(t *testing.T) {
	for _, cfg := range []PCConfig{
		{Producers: 1, Consumers: 1, Capacity: 1, Items: 20},
		{Producers: 3, Consumers: 2, Capacity: 2, Items: 15, Delay: time.Millisecond},
		{Producers: 2, Consumers: 4, Capacity: 5, Items: 25, Delay: time.Millisecond},
	} {
		r, err := ProducerConsumer(cfg)
		if err != nil {
			t.Fatal(err)
		}
		missing, duplicated := checkDelivery(r)
		consumed := 0
		for _, n := range r.Consumed {
			consumed += n
		}
		if missing != 0 || duplicated != 0 || consumed != cfg.Producers*cfg.Items {
			t.Errorf("%+v: consumed %d, missing %d, duplicated %d", cfg, consumed, missing, duplicated)
		}
		if r.MaxOccupancy > cfg.Capacity || r.Overflows != 0 || r.Underflows != 0 {
			t.Errorf("%+v: peak %d, overflows %d, underflows %d", cfg, r.MaxOccupancy, r.Overflows, r.Underflows)
		}
	}
}

func TestProducerConsumerKeepsPerProducerOrder(t *testing.T) {
	// The buffer is FIFO, so with one consumer each producer's items must
	// come out in the order they were made.
	r, err := ProducerConsumer(PCConfig{Producers: 3, Consumers: 1, Capacity: 2, Items: 10})
	if err != nil {
		t.Fatal(err)
	}
	last := map[int]int{}
	for _, it := range r.Received[0] {
		if it.Seq != last[it.Producer]+1 {
			t.Fatalf("producer %d: item %d after item %d", it.Producer, it.Seq, last[it.Producer])
		}
		last[it.Producer] = it.Seq
	}
}

func TestProducersBlockOnFullBuffer(t *testing.T) {
	// Fast producers and a slow consumer: the buffer fills up and the
	// producers have to wait on the "empty slots" semaphore.
	r, err := ProducerConsumer(PCConfig{Producers: 3, Consumers: 1, Capacity: 2, Items: 4})
	if err != nil {
		t.Fatal(err)
	}
	if r.MaxOccupancy != 2 || r.FullWaits == 0 {
		t.Errorf("peak occupancy %d, producer waits %d; want a full buffer and waiting producers", r.MaxOccupancy, r.FullWaits)
	}
}

func TestProducerConsumerInvalidConfig(t *testing.T) {
	for _, cfg := range []PCConfig{
		{Producers: 0, Consumers: 1, Capacity: 1},
		{Producers: 1, Consumers: 0, Capacity: 1},
		{Producers: 1, Consumers: 1, Capacity: 0},
		{Producers: 1, Consumers: 1, Capacity: 1, Items: -1},
	} {
		if _, err := ProducerConsumer(cfg); err == nil {
			t.Errorf("%+v: expected an error", cfg)
		}
	}
}

func TestDiningPhilosophersWithoutDeadlock(t *testing.T) {
	for _, s := range []DiningStrategy{DiningOrdered, DiningWaiter} {
		for _, n := range []int{2, 5, 7} {
			cfg := DiningConfig{
				Philosophers: n, Meals: 4, Strategy: s,
				Think: time.Millisecond, Eat: time.Millisecond, Reach: time.Millisecond,
			}
			r, err := runWithTimeout(t, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if r.Deadlocked {
				t.Fatalf("%s, %d philosophers: deadlocked", s, n)
			}
			for i, m := range r.Meals {
				if m != cfg.Meals {
					t.Errorf("%s, %d philosophers: philosopher %d ate %d meals, want %d", s, n, i, m, cfg.Meals)
				}
			}
			if r.NeighborViolations != 0 || r.MaxEating > n/2 {
				t.Errorf("%s, %d philosophers: %d neighbour violations, %d eating at once",
					s, n, r.NeighborViolations, r.MaxEating)
			}
		}
	}
}

func TestNaiveDiningPhilosophersDeadlock(t *testing.T) {
	// Everyone starts hungry and pauses between forks, so they all hold their
	// left fork when they reach for the right one.
	cfg := DiningConfig{Philosophers: 5, Meals: 1, Strategy: DiningNaive, Reach: 20 * time.Millisecond}
	r, err := runWithTimeout(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Deadlocked {
		t.Errorf("naive strategy did not deadlock: meals %v", r.Meals)
	}
}

func TestDiningInvalidConfig(t *testing.T) {
	if _, err := DiningPhilosophers(DiningConfig{Philosophers: 1, Meals: 1}); err == nil {
		t.Error("1 philosopher: expected an error")
	}
	if _, err := DiningPhilosophers(DiningConfig{Philosophers: 5, Meals: 0}); err == nil {
		t.Error("0 meals: expected an error")
	}
	if _, err := ParseDiningStrategy("greedy"); err == nil {
		t.Error("unknown strategy: expected an error")
	}
}

// runWithTimeout fails the test instead of hanging if a run never returns.
func runWithTimeout(t *testing.T, cfg DiningConfig) (DiningResult, error) {
	t.Helper()
	type out struct {
		r   DiningResult
		err error
	}
	ch := make(chan out, 1)
	go func() {
		r, err := DiningPhilosophers(cfg)
		ch <- out{r, err}
	}()
	select {
	case o := <-ch:
		return o.r, o.err
	case <-time.After(10 * time.Second):
		t.Fatalf("%s strategy with %d philosophers did not finish", cfg.Strategy, cfg.Philosophers)
		return DiningResult{}, nil
	}
}
