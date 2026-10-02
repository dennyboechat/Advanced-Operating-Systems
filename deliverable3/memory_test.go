package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// faults runs a single-process reference string and returns the page faults.
func faults(t *testing.T, refs string, frames int, policy Replacement) int {
	t.Helper()
	ops, err := referenceString(refs)
	if err != nil {
		t.Fatalf("referenceString: %v", err)
	}
	m, err := NewMemory(frames, 4096, policy)
	if err != nil {
		t.Fatalf("NewMemory: %v", err)
	}
	if err := (&memRunner{m: m}).run(ops); err != nil {
		t.Fatalf("run: %v", err)
	}
	return m.Faults
}

// The reference strings and fault counts are the standard textbook examples
// (Silberschatz, Operating System Concepts, ch. 10).
const (
	textbookRefs = "7 0 1 2 0 3 0 4 2 3 0 3 2 1 2 0 1 7 0 1"
	beladyRefs   = "1 2 3 4 1 2 5 1 2 3 4 5"
)

func TestFIFOTextbookExample(t *testing.T) {
	if got := faults(t, textbookRefs, 3, ReplaceFIFO); got != 15 {
		t.Errorf("FIFO, 3 frames: %d faults, want 15", got)
	}
}

func TestLRUTextbookExample(t *testing.T) {
	if got := faults(t, textbookRefs, 3, ReplaceLRU); got != 12 {
		t.Errorf("LRU, 3 frames: %d faults, want 12", got)
	}
}

func TestBeladysAnomaly(t *testing.T) {
	three := faults(t, beladyRefs, 3, ReplaceFIFO)
	four := faults(t, beladyRefs, 4, ReplaceFIFO)
	if three != 9 || four != 10 {
		t.Errorf("FIFO faults with 3 and 4 frames: %d and %d, want 9 and 10", three, four)
	}
	// LRU is a stack algorithm: more frames never means more faults.
	prev := faults(t, beladyRefs, 1, ReplaceLRU)
	for frames := 2; frames <= 6; frames++ {
		got := faults(t, beladyRefs, frames, ReplaceLRU)
		if got > prev {
			t.Errorf("LRU faulted more with %d frames (%d) than with %d (%d)", frames, got, frames-1, prev)
		}
		prev = got
	}
}

func TestFIFOAndLRUChooseDifferentVictims(t *testing.T) {
	// Pages 0, 1, 2 fill memory; 0 is used again, then 3 needs a frame.
	// FIFO evicts 0 (loaded first), LRU evicts 1 (used least recently).
	for _, tc := range []struct {
		policy Replacement
		victim int
	}{{ReplaceFIFO, 0}, {ReplaceLRU, 1}} {
		m, _ := NewMemory(3, 4096, tc.policy)
		m.Allocate("P1", 4)
		for _, p := range []int{0, 1, 2, 0} {
			m.Access("P1", p)
		}
		res, err := m.Access("P1", 3)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Fault || res.Victim == nil || res.Victim.Page != tc.victim {
			t.Errorf("%s: replaced %v, want P1:%d", tc.policy, res.Victim, tc.victim)
		}
	}
}

func TestPageFaultLoadsIntoFreeFrameFirst(t *testing.T) {
	m, _ := NewMemory(3, 4096, ReplaceFIFO)
	m.Allocate("P1", 5)
	for i, page := range []int{4, 2, 0} {
		res, _ := m.Access("P1", page)
		if !res.Fault || res.Victim != nil || res.Frame != i {
			t.Errorf("page %d: fault=%v victim=%v frame=%d, want a fault into free frame %d",
				page, res.Fault, res.Victim, res.Frame, i)
		}
	}
	if res, _ := m.Access("P1", 2); res.Fault {
		t.Errorf("page 2 is resident, the second reference should be a hit")
	}
	if m.Replacements != 0 {
		t.Errorf("%d replacements while memory had free frames", m.Replacements)
	}
}

func TestGlobalReplacementAcrossProcesses(t *testing.T) {
	m, _ := NewMemory(2, 1024, ReplaceFIFO)
	m.Allocate("A", 2)
	m.Allocate("B", 2)
	m.Access("A", 0)
	m.Access("A", 1)
	res, _ := m.Access("B", 0)
	if res.Victim == nil || res.Victim.PID != "A" || res.Victim.Page != 0 {
		t.Fatalf("B's fault replaced %v, want A:0", res.Victim)
	}
	a, _ := m.Process("A")
	b, _ := m.Process("B")
	if a.Resident != 1 || a.Evicted != 1 || a.PageTable[0].Valid {
		t.Errorf("A: resident %d, evicted %d, page 0 valid %v; want 1, 1, false", a.Resident, a.Evicted, a.PageTable[0].Valid)
	}
	if b.Resident != 1 || b.Faults != 1 {
		t.Errorf("B: resident %d, faults %d; want 1 and 1", b.Resident, b.Faults)
	}
	// A references the page it lost: another fault.
	if res, _ := m.Access("A", 0); !res.Fault {
		t.Errorf("A:0 was evicted, referencing it again should fault")
	}
}

func TestFreeReleasesFrames(t *testing.T) {
	m, _ := NewMemory(4, 4096, ReplaceLRU)
	m.Allocate("A", 3)
	m.Allocate("B", 2)
	m.Access("A", 0)
	m.Access("B", 0)
	m.Access("A", 1)
	m.Access("B", 1)

	released, err := m.Free("A")
	if err != nil || released != 2 {
		t.Fatalf("Free(A) = %d, %v; want 2 frames released", released, err)
	}
	if m.Used() != 2 {
		t.Errorf("%d frames in use after freeing A, want 2", m.Used())
	}
	// The next fault takes a freed frame instead of replacing a page of B.
	m.Allocate("C", 1)
	if res, _ := m.Access("C", 0); res.Victim != nil {
		t.Errorf("C replaced %v although a frame was free", res.Victim)
	}
	if _, err := m.Access("A", 0); err == nil {
		t.Errorf("accessing a freed process should fail")
	}
	if _, err := m.Free("A"); err == nil {
		t.Errorf("freeing a process twice should fail")
	}
	// A freed PID can be allocated again and starts afresh.
	if err := m.Allocate("A", 1); err != nil {
		t.Errorf("re-allocating freed A: %v", err)
	}
	if a, _ := m.Process("A"); a.Accesses != 0 || a.Freed {
		t.Errorf("re-allocated A kept old state: %+v", a)
	}
}

func TestMemoryUsageTracking(t *testing.T) {
	m, _ := NewMemory(3, 2048, ReplaceFIFO)
	m.Allocate("P1", 4)
	for _, p := range []int{0, 1, 2, 0, 3} {
		m.Access("P1", p)
	}
	p, _ := m.Process("P1")
	if p.Accesses != 5 || p.Hits != 1 || p.Faults != 4 {
		t.Errorf("accesses/hits/faults = %d/%d/%d, want 5/1/4", p.Accesses, p.Hits, p.Faults)
	}
	if p.Resident != 3 || p.PeakResident != 3 || p.Evicted != 1 {
		t.Errorf("resident %d, peak %d, evicted %d; want 3, 3, 1", p.Resident, p.PeakResident, p.Evicted)
	}
	if m.Demand() != 4 || m.Demand() <= len(m.Frames) {
		t.Errorf("demand %d pages for %d frames should be overcommitted", m.Demand(), len(m.Frames))
	}
	if m.Hits+m.Faults != m.Accesses {
		t.Errorf("hits %d + faults %d != accesses %d", m.Hits, m.Faults, m.Accesses)
	}
}

func TestSegmentationFault(t *testing.T) {
	m, _ := NewMemory(2, 4096, ReplaceFIFO)
	m.Allocate("P1", 2)
	for _, page := range []int{2, -1} {
		_, err := m.Access("P1", page)
		var segv *SegmentationFault
		if !errors.As(err, &segv) {
			t.Errorf("page %d: got %v, want a segmentation fault", page, err)
		}
	}
	p, _ := m.Process("P1")
	if p.Invalid != 2 || m.Accesses != 0 || m.Faults != 0 {
		t.Errorf("invalid %d, accesses %d, faults %d; want 2, 0, 0", p.Invalid, m.Accesses, m.Faults)
	}
	// A segfault in a workload is reported but does not stop the run.
	ops, _ := parseInline("alloc P1 2; P1 9; P1 0")
	m2, _ := NewMemory(2, 4096, ReplaceFIFO)
	if err := (&memRunner{m: m2}).run(ops); err != nil || m2.Accesses != 1 {
		t.Errorf("run: %v, %d accesses; want no error and 1 access", err, m2.Accesses)
	}
}

func TestInvalidMemoryOperations(t *testing.T) {
	if _, err := NewMemory(0, 4096, ReplaceFIFO); err == nil {
		t.Error("0 frames: expected an error")
	}
	if _, err := NewMemory(4, 0, ReplaceFIFO); err == nil {
		t.Error("page size 0: expected an error")
	}
	m, _ := NewMemory(2, 4096, ReplaceFIFO)
	if err := m.Allocate("P1", 0); err == nil {
		t.Error("0 pages: expected an error")
	}
	m.Allocate("P1", 1)
	if err := m.Allocate("P1", 1); err == nil {
		t.Error("allocating P1 twice: expected an error")
	}
	if _, err := m.Access("P9", 0); err == nil {
		t.Error("unknown process: expected an error")
	}
	ops, _ := parseInline("P9 0")
	if err := (&memRunner{m: m}).run(ops); err == nil {
		t.Error("workload referencing an unknown process: expected an error")
	}
}

func TestParseMemOp(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want MemOp
	}{
		{"alloc P1 4", MemOp{Kind: OpAlloc, PID: "P1", Count: 4}},
		{"access P1 0 2 1", MemOp{Kind: OpAccess, PID: "P1", Pages: []int{0, 2, 1}}},
		{"P2 3", MemOp{Kind: OpAccess, PID: "P2", Pages: []int{3}}},
		{"free P1", MemOp{Kind: OpFree, PID: "P1"}},
		{"frames", MemOp{Kind: OpFrames}},
		{"stats", MemOp{Kind: OpStats}},
	} {
		got, err := parseMemOp(tc.in)
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if got.Kind != tc.want.Kind || got.PID != tc.want.PID || got.Count != tc.want.Count ||
			len(got.Pages) != len(tc.want.Pages) {
			t.Errorf("%q: got %+v, want %+v", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"alloc P1", "alloc P1 x", "free", "P1", "P1 a", "access P1"} {
		if _, err := parseMemOp(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestReferenceStringSizesTheProcess(t *testing.T) {
	ops, err := referenceString("3, 0, 7, 1")
	if err != nil {
		t.Fatal(err)
	}
	if ops[0].Kind != OpAlloc || ops[0].Count != 8 || len(ops[1].Pages) != 4 {
		t.Errorf("got %+v, want P1 allocated 8 pages and 4 references", ops)
	}
	if _, err := referenceString("1 -2"); err == nil {
		t.Error("negative page: expected an error")
	}
}

func TestDefaultWorkloadOvercommitsMemory(t *testing.T) {
	ops, err := parseInline(defaultWorkload)
	if err != nil {
		t.Fatalf("default workload does not parse: %v", err)
	}
	for _, policy := range []Replacement{ReplaceFIFO, ReplaceLRU} {
		m, _ := NewMemory(4, 4096, policy)
		var out bytes.Buffer
		if err := (&memRunner{m: m, out: &out, trace: true}).run(ops); err != nil {
			t.Fatalf("%s: %v", policy, err)
		}
		if m.Replacements == 0 || !strings.Contains(out.String(), "overcommitted") {
			t.Errorf("%s: the sample workload should overflow memory and need replacement", policy)
		}
	}
}

func TestInteractivePrompt(t *testing.T) {
	m, _ := NewMemory(2, 4096, ReplaceLRU)
	in := strings.NewReader("alloc P1 3\nP1 0 1 0 2\nbogus command\nP1 7\nfree P1\nquit\n")
	var out bytes.Buffer
	memPrompt(m, in, &out)
	text := out.String()
	for _, want := range []string{
		"LRU replaces P1:1", // 0 was used after 1, so 1 is the LRU page
		"error:",            // the bad line is reported, the prompt goes on
		"SEGFAULT",
		"P1 freed, 2 frame(s) released",
		"Page faults:      3",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output does not contain %q:\n%s", want, text)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[int]string{512: "512 B", 4096: "4 KB", 1536: "1.5 KB", 1 << 20: "1 MB"} {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
