package main

import "sync"

// Semaphore is a counting semaphore: a non-negative counter with two atomic
// operations. Wait (Dijkstra's P) takes one unit, blocking while the counter
// is zero; Signal (V) gives one back and wakes a waiter.
//
// It is built on a condition variable rather than on a Go channel so that the
// blocking is explicit: a waiter sleeps in cond.Wait until Signal changes the
// counter, and re-checks the counter when it wakes up.
type Semaphore struct {
	mu     sync.Mutex
	cond   *sync.Cond
	value  int
	closed bool
	blocks int // times a caller found the counter at zero and had to sleep
}

// NewSemaphore returns a semaphore whose counter starts at value.
func NewSemaphore(value int) *Semaphore {
	if value < 0 {
		panic("semaphore: negative initial value")
	}
	s := &Semaphore{value: value}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// Wait decrements the counter, blocking while it is zero. It returns false,
// without taking a unit, if the semaphore was closed while it waited.
func (s *Semaphore) Wait() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.value == 0 && !s.closed {
		s.blocks++
	}
	for s.value == 0 && !s.closed {
		s.cond.Wait()
	}
	if s.closed {
		return false
	}
	s.value--
	return true
}

// TryWait decrements the counter if it is positive and reports whether it did.
// It never blocks.
func (s *Semaphore) TryWait() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.value == 0 || s.closed {
		return false
	}
	s.value--
	return true
}

// Signal increments the counter and wakes one waiter, if there is any.
func (s *Semaphore) Signal() {
	s.mu.Lock()
	s.value++
	s.mu.Unlock()
	s.cond.Signal()
}

// Close wakes every waiter and makes all later Waits fail. It is how a stuck
// set of processes (a deadlock) is torn down.
func (s *Semaphore) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cond.Broadcast()
}

// Value returns the current counter.
func (s *Semaphore) Value() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.value
}

// Blocks returns how many times a Wait had to sleep because the counter was zero.
func (s *Semaphore) Blocks() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blocks
}

// Mutex is a binary semaphore used for mutual exclusion: at most one holder
// at a time. Unlocking a mutex that is not locked is a bug and panics.
type Mutex struct {
	sem *Semaphore
}

// NewMutex returns an unlocked mutex.
func NewMutex() *Mutex { return &Mutex{sem: NewSemaphore(1)} }

// Lock acquires the mutex, blocking while another holder has it. It returns
// false if the mutex was closed while it waited.
func (m *Mutex) Lock() bool { return m.sem.Wait() }

// TryLock acquires the mutex only if it is free.
func (m *Mutex) TryLock() bool { return m.sem.TryWait() }

// Unlock releases the mutex.
func (m *Mutex) Unlock() {
	s := m.sem
	s.mu.Lock()
	if s.value != 0 {
		s.mu.Unlock()
		panic("mutex: unlock of unlocked mutex")
	}
	s.value++
	s.mu.Unlock()
	s.cond.Signal()
}

// Close wakes every goroutine blocked in Lock; see Semaphore.Close.
func (m *Mutex) Close() { m.sem.Close() }

// Blocks returns how many times a Lock had to wait for the holder.
func (m *Mutex) Blocks() int { return m.sem.Blocks() }
