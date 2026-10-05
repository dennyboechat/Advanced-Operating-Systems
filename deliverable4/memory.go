package main

import (
	"fmt"
	"strings"
)

// Replacement selects the page replacement algorithm used when every frame is
// taken and a process references a page that is not in memory.
type Replacement int

const (
	ReplaceFIFO Replacement = iota // evict the page that was loaded first
	ReplaceLRU                     // evict the page that was used least recently
)

func (r Replacement) String() string {
	if r == ReplaceLRU {
		return "LRU"
	}
	return "FIFO"
}

// ParseReplacement turns the -algo flag value into a replacement algorithm.
func ParseReplacement(s string) (Replacement, error) {
	switch strings.ToLower(s) {
	case "fifo":
		return ReplaceFIFO, nil
	case "lru":
		return ReplaceLRU, nil
	default:
		return 0, fmt.Errorf("unknown page replacement algorithm %q (use \"fifo\" or \"lru\")", s)
	}
}

// Frame is one fixed-size page frame of physical memory.
type Frame struct {
	Used     bool
	PID      string // owner of the page held in the frame
	Page     int    // virtual page number held in the frame
	LoadedAt int    // clock value when the page was brought in (FIFO)
	LastUsed int    // clock value of the latest reference to the page (LRU)
}

// PageTableEntry maps one virtual page of a process to a frame.
type PageTableEntry struct {
	Valid bool // the page is resident; Frame is meaningful only when set
	Frame int
}

// MemProcess is a process as the memory manager sees it: a page table and the
// counters that track its memory usage.
type MemProcess struct {
	PID       string
	PageTable []PageTableEntry // one entry per virtual page

	Resident     int // pages currently in memory
	PeakResident int // most pages it ever had in memory at once
	Accesses     int // valid page references
	Hits         int // references to a page that was already resident
	Faults       int // references to a page that had to be loaded
	Evicted      int // times one of its pages was replaced to make room
	Invalid      int // references outside its address space
	Freed        bool
}

// Pages is the size of the process's virtual address space, in pages.
func (p *MemProcess) Pages() int { return len(p.PageTable) }

// FaultRate is the fraction of references that caused a page fault, in percent.
func (p *MemProcess) FaultRate() float64 { return percent(p.Faults, p.Accesses) }

// PageRef names one virtual page of one process.
type PageRef struct {
	PID  string
	Page int
}

func (r PageRef) String() string { return fmt.Sprintf("%s:%d", r.PID, r.Page) }

// SegmentationFault is returned for a reference to a page outside the
// process's address space. The reference is counted but nothing is loaded.
type SegmentationFault struct {
	PID   string
	Page  int
	Pages int // size of the address space
}

func (e *SegmentationFault) Error() string {
	return fmt.Sprintf("segmentation fault: process %s has pages 0-%d, page %d is outside its address space",
		e.PID, e.Pages-1, e.Page)
}

// AccessResult describes what a single page reference did.
type AccessResult struct {
	Time   int // clock value of the reference
	PID    string
	Page   int
	Fault  bool     // the page was not resident
	Frame  int      // frame that holds the page afterwards
	Victim *PageRef // page evicted to make room, nil if a free frame was used
}

// Memory is a paged physical memory of fixed-size frames shared by every
// process. Pages are loaded on demand: allocating a process only builds its
// page table, and a page is brought into a frame the first time it is
// referenced. Replacement is global, so a process can evict another's pages.
type Memory struct {
	PageSize int
	Policy   Replacement
	Frames   []Frame

	procs map[string]*MemProcess
	order []string // PIDs in allocation order, for reporting
	clock int      // advances by one on every reference

	Accesses     int
	Hits         int
	Faults       int
	Replacements int // faults that found memory full and evicted a page
	PeakUsed     int // most frames in use at once
}

// NewMemory creates a memory of frames page frames of pageSize bytes each.
func NewMemory(frames, pageSize int, policy Replacement) (*Memory, error) {
	if frames < 1 {
		return nil, fmt.Errorf("memory needs at least 1 frame, got %d", frames)
	}
	if pageSize < 1 {
		return nil, fmt.Errorf("page size must be at least 1 byte, got %d", pageSize)
	}
	return &Memory{
		PageSize: pageSize,
		Policy:   policy,
		Frames:   make([]Frame, frames),
		procs:    make(map[string]*MemProcess),
	}, nil
}

// Allocate gives a new process a virtual address space of pages pages. None of
// them is resident yet; each is loaded by the page fault its first reference
// causes. A PID that was freed can be allocated again and starts afresh.
func (m *Memory) Allocate(pid string, pages int) error {
	if pid == "" {
		return fmt.Errorf("a process needs a name")
	}
	if pages < 1 {
		return fmt.Errorf("process %s: needs at least 1 page, got %d", pid, pages)
	}
	if p, ok := m.procs[pid]; ok && !p.Freed {
		return fmt.Errorf("process %s already has memory allocated", pid)
	} else if !ok {
		m.order = append(m.order, pid)
	}
	m.procs[pid] = &MemProcess{PID: pid, PageTable: make([]PageTableEntry, pages)}
	return nil
}

// Free deallocates every page of a process and releases the frames it held.
// It returns the number of frames that became free.
func (m *Memory) Free(pid string) (int, error) {
	p, err := m.live(pid)
	if err != nil {
		return 0, err
	}
	released := 0
	for page, pte := range p.PageTable {
		if pte.Valid {
			m.Frames[pte.Frame] = Frame{}
			p.PageTable[page] = PageTableEntry{}
			released++
		}
	}
	p.Resident = 0
	p.Freed = true
	return released, nil
}

// Access makes process pid reference one of its virtual pages. A resident page
// is a hit. Otherwise it is a page fault: the page is loaded into a free frame
// or, when memory is full, into the frame of a victim chosen by the
// replacement algorithm.
func (m *Memory) Access(pid string, page int) (AccessResult, error) {
	p, err := m.live(pid)
	if err != nil {
		return AccessResult{}, err
	}
	if page < 0 || page >= p.Pages() {
		p.Invalid++
		return AccessResult{}, &SegmentationFault{PID: pid, Page: page, Pages: p.Pages()}
	}

	m.clock++
	m.Accesses++
	p.Accesses++
	res := AccessResult{Time: m.clock, PID: pid, Page: page}

	if pte := p.PageTable[page]; pte.Valid {
		m.Hits++
		p.Hits++
		m.Frames[pte.Frame].LastUsed = m.clock
		res.Frame = pte.Frame
		return res, nil
	}

	// Page fault.
	m.Faults++
	p.Faults++
	res.Fault = true

	frame := m.freeFrame()
	if frame < 0 {
		frame = m.victim()
		old := m.Frames[frame]
		owner := m.procs[old.PID]
		owner.PageTable[old.Page] = PageTableEntry{}
		owner.Resident--
		owner.Evicted++
		m.Replacements++
		res.Victim = &PageRef{PID: old.PID, Page: old.Page}
	}

	m.Frames[frame] = Frame{Used: true, PID: pid, Page: page, LoadedAt: m.clock, LastUsed: m.clock}
	p.PageTable[page] = PageTableEntry{Valid: true, Frame: frame}
	p.Resident++
	p.PeakResident = max(p.PeakResident, p.Resident)
	m.PeakUsed = max(m.PeakUsed, m.Used())
	res.Frame = frame
	return res, nil
}

// freeFrame returns the lowest-numbered empty frame, or -1 when memory is full.
func (m *Memory) freeFrame() int {
	for i, f := range m.Frames {
		if !f.Used {
			return i
		}
	}
	return -1
}

// victim picks the frame to replace. Memory is full whenever it is called, so
// every frame holds a page. FIFO evicts the page that has been in memory the
// longest, whether or not it is still being used; LRU evicts the page whose
// last reference is the oldest. Clock values are unique, so there are no ties.
func (m *Memory) victim() int {
	key := func(f Frame) int {
		if m.Policy == ReplaceLRU {
			return f.LastUsed
		}
		return f.LoadedAt
	}
	best := 0
	for i := 1; i < len(m.Frames); i++ {
		if key(m.Frames[i]) < key(m.Frames[best]) {
			best = i
		}
	}
	return best
}

// live returns the process pid if it exists and has not been freed.
func (m *Memory) live(pid string) (*MemProcess, error) {
	p, ok := m.procs[pid]
	switch {
	case !ok:
		return nil, fmt.Errorf("process %s has no memory allocated (use \"alloc %s <pages>\" first)", pid, pid)
	case p.Freed:
		return nil, fmt.Errorf("process %s has already been freed", pid)
	}
	return p, nil
}

// Process returns the memory record of pid, freed or not.
func (m *Memory) Process(pid string) (*MemProcess, bool) {
	p, ok := m.procs[pid]
	return p, ok
}

// Processes returns every process that was ever allocated, in allocation order.
func (m *Memory) Processes() []*MemProcess {
	out := make([]*MemProcess, len(m.order))
	for i, pid := range m.order {
		out[i] = m.procs[pid]
	}
	return out
}

// Used is the number of frames that currently hold a page.
func (m *Memory) Used() int {
	n := 0
	for _, f := range m.Frames {
		if f.Used {
			n++
		}
	}
	return n
}

// Demand is the total virtual memory, in pages, of the processes that are
// still allocated. When it exceeds the number of frames, memory is
// overcommitted and page replacement will be needed.
func (m *Memory) Demand() int {
	n := 0
	for _, p := range m.procs {
		if !p.Freed {
			n += p.Pages()
		}
	}
	return n
}

// HitRate is the fraction of references that found their page resident, in percent.
func (m *Memory) HitRate() float64 { return percent(m.Hits, m.Accesses) }

// FaultRate is the fraction of references that caused a page fault, in percent.
func (m *Memory) FaultRate() float64 { return percent(m.Faults, m.Accesses) }

func percent(part, whole int) float64 {
	if whole == 0 {
		return 0
	}
	return float64(part) / float64(whole) * 100
}
