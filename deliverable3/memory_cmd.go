package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// runMem is the "mem" subcommand: a paging simulator driven by a workload of
// alloc / access / free operations, or by an interactive prompt.
func runMem(args []string) {
	fs := flag.NewFlagSet("mem", flag.ExitOnError)
	algo := fs.String("algo", "fifo", "page replacement: \"fifo\", \"lru\", or \"both\" to compare them")
	frames := fs.Int("frames", 4, "number of physical page frames")
	pageSize := fs.Int("pagesize", 4096, "page and frame size in bytes")
	file := fs.String("file", "", "workload file, one operation per line (\"-\" reads standard input)")
	ops := fs.String("ops", "", "inline workload, operations separated by \";\", e.g. \"alloc P1 3; access P1 0 1 2\"")
	refs := fs.String("refs", "", "reference string for a single process P1, e.g. \"7 0 1 2 0 3\"")
	interactive := fs.Bool("i", false, "interactive: type operations at a mem> prompt")
	sweep := fs.Int("sweep", 0, "also compare FIFO and LRU page faults for 1..N frames")
	quiet := fs.Bool("quiet", false, "print only the summary, not every reference")
	fs.Usage = func() { memUsage(fs) }
	fs.Parse(args)

	policies, err := parsePolicies(*algo)
	if err != nil {
		fail(err)
	}

	if *interactive {
		if len(policies) != 1 {
			fail(fmt.Errorf("-i runs one algorithm at a time, pick -algo fifo or -algo lru"))
		}
		if *file != "" || *ops != "" || *refs != "" {
			fail(fmt.Errorf("-i reads operations from the prompt, drop -file, -ops and -refs"))
		}
		m, err := NewMemory(*frames, *pageSize, policies[0])
		if err != nil {
			fail(err)
		}
		printMemHeader(m)
		memPrompt(m, os.Stdin, os.Stdout)
		return
	}

	workload, source, err := loadWorkload(*file, *ops, *refs)
	if err != nil {
		fail(err)
	}

	var runs []*Memory
	for _, policy := range policies {
		m, err := NewMemory(*frames, *pageSize, policy)
		if err != nil {
			fail(err)
		}
		printMemHeader(m)
		fmt.Printf("Workload: %d operation(s) (%s)\n\n", len(workload), source)
		r := &memRunner{m: m, out: os.Stdout, trace: !*quiet}
		if !*quiet {
			fmt.Println("Trace")
		}
		if err := r.run(workload); err != nil {
			fail(err)
		}
		// frames and stats already end with a blank line.
		if last := workload[len(workload)-1].Kind; !*quiet && last != OpFrames && last != OpStats {
			fmt.Println()
		}
		printMemReport(os.Stdout, m)
		fmt.Println()
		runs = append(runs, m)
	}

	if len(runs) > 1 {
		printComparison(runs)
	}
	if *sweep > 0 {
		if err := printSweep(workload, *sweep, *pageSize); err != nil {
			fail(err)
		}
	}
}

func parsePolicies(algo string) ([]Replacement, error) {
	if strings.ToLower(algo) == "both" {
		return []Replacement{ReplaceFIFO, ReplaceLRU}, nil
	}
	r, err := ParseReplacement(algo)
	if err != nil {
		return nil, fmt.Errorf("%v, or \"both\"", err)
	}
	return []Replacement{r}, nil
}

func memUsage(fs *flag.FlagSet) {
	out := fs.Output()
	name := filepath.Base(os.Args[0]) + " mem"
	fmt.Fprintf(out, "Usage: %s [flags]\n\n", name)
	fmt.Fprintln(out, "Simulates demand paging over fixed-size page frames. Processes are given")
	fmt.Fprintln(out, "pages, a reference to a page that is not resident is a page fault, and when")
	fmt.Fprintln(out, "every frame is taken FIFO or LRU picks the page to replace.")
	fmt.Fprintln(out, "\nWorkload operations (one per line in -file, \";\"-separated in -ops):")
	fmt.Fprintln(out, "  alloc <pid> <pages>      give a process a virtual address space")
	fmt.Fprintln(out, "  access <pid> <page>...   reference pages (\"access\" may be left out)")
	fmt.Fprintln(out, "  free <pid>               deallocate a process, releasing its frames")
	fmt.Fprintln(out, "  frames                   print the memory map")
	fmt.Fprintln(out, "  stats                    print the per-process usage table")
	fmt.Fprintln(out, "\nWith no workload flag, the built-in sample workload is used.")
	fmt.Fprintln(out, "\nFlags:")
	fs.PrintDefaults()
	fmt.Fprintln(out, "\nExamples:")
	fmt.Fprintf(out, "  %s -algo fifo -frames 4 -file workload.txt\n", name)
	fmt.Fprintf(out, "  %s -algo both -frames 3 -refs \"1 2 3 4 1 2 5 1 2 3 4 5\" -sweep 5\n", name)
	fmt.Fprintf(out, "  %s -algo lru -ops \"alloc A 3; alloc B 2; A 0 1; B 0 1; A 2 0\"\n", name)
	fmt.Fprintf(out, "  %s -i -algo lru -frames 3\n", name)
}

// MemOpKind is the kind of a workload operation.
type MemOpKind int

const (
	OpAlloc MemOpKind = iota
	OpAccess
	OpFree
	OpFrames
	OpStats
)

// MemOp is one workload operation.
type MemOp struct {
	Kind  MemOpKind
	PID   string
	Count int   // OpAlloc: pages to allocate
	Pages []int // OpAccess: pages referenced, in order
}

// parseMemOp parses one operation, e.g. "alloc P1 4", "access P1 0 1 2",
// "P1 0 1 2", "free P1", "frames" or "stats".
func parseMemOp(text string) (MemOp, error) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return MemOp{}, fmt.Errorf("empty operation")
	}
	switch cmd := strings.ToLower(fields[0]); cmd {
	case "alloc":
		if len(fields) != 3 {
			return MemOp{}, fmt.Errorf("expected \"alloc <pid> <pages>\"")
		}
		n, err := strconv.Atoi(fields[2])
		if err != nil {
			return MemOp{}, fmt.Errorf("page count %q is not a number", fields[2])
		}
		return MemOp{Kind: OpAlloc, PID: fields[1], Count: n}, nil
	case "free":
		if len(fields) != 2 {
			return MemOp{}, fmt.Errorf("expected \"free <pid>\"")
		}
		return MemOp{Kind: OpFree, PID: fields[1]}, nil
	case "frames", "memory":
		return MemOp{Kind: OpFrames}, nil
	case "stats":
		return MemOp{Kind: OpStats}, nil
	case "access":
		fields = fields[1:]
	}

	// "[access] <pid> <page> [<page>...]"
	if len(fields) < 2 {
		return MemOp{}, fmt.Errorf("expected \"access <pid> <page>...\"")
	}
	pages, err := parsePages(fields[1:])
	if err != nil {
		return MemOp{}, err
	}
	return MemOp{Kind: OpAccess, PID: fields[0], Pages: pages}, nil
}

func parsePages(fields []string) ([]int, error) {
	pages := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil {
			return nil, fmt.Errorf("page number %q is not a number", f)
		}
		pages = append(pages, n)
	}
	return pages, nil
}

// defaultWorkload is used when no workload flag is given: three processes
// whose combined address space (8 pages) is larger than the default four
// frames. P1 keeps returning to its page 0, which LRU keeps resident and FIFO
// keeps evicting.
const defaultWorkload = `alloc P1 4; alloc P2 2; alloc P3 2;
P1 0 1 2; P1 0; P2 0; P1 0 1; P3 0; P1 0 3; P2 0 1; P1 0 1; P3 0 1; P1 0 2;
free P2; P3 0 1; P1 0 3 0`

// loadWorkload reads the operations from a file, the inline list or a
// reference string, or falls back to the built-in sample.
func loadWorkload(file, ops, refs string) ([]MemOp, string, error) {
	given := 0
	for _, s := range []string{file, ops, refs} {
		if s != "" {
			given++
		}
	}
	if given > 1 {
		return nil, "", fmt.Errorf("use only one of -file, -ops and -refs")
	}

	switch {
	case file == "-":
		w, err := parseWorkload(os.Stdin, "stdin")
		return w, "from standard input", err
	case file != "":
		f, err := os.Open(file)
		if err != nil {
			return nil, "", err
		}
		defer f.Close()
		w, err := parseWorkload(f, file)
		return w, "from " + file, err
	case ops != "":
		w, err := parseInline(ops)
		return w, "from -ops", err
	case refs != "":
		w, err := referenceString(refs)
		return w, "reference string for P1", err
	default:
		w, err := parseInline(defaultWorkload)
		return w, "built-in sample workload", err
	}
}

// parseWorkload reads one operation per line. Blank lines and lines starting
// with # are ignored.
func parseWorkload(r io.Reader, name string) ([]MemOp, error) {
	var ops []MemOp
	scanner := bufio.NewScanner(r)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		op, err := parseMemOp(text)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %v", name, line, err)
		}
		ops = append(ops, op)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("%s contains no operations", name)
	}
	return ops, nil
}

// parseInline parses operations separated by semicolons or newlines.
func parseInline(list string) ([]MemOp, error) {
	var ops []MemOp
	for _, entry := range strings.FieldsFunc(list, func(r rune) bool { return r == ';' || r == '\n' }) {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		op, err := parseMemOp(entry)
		if err != nil {
			return nil, fmt.Errorf("%q: %v", entry, err)
		}
		ops = append(ops, op)
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("-ops is empty")
	}
	return ops, nil
}

// referenceString turns a textbook reference string such as "7 0 1 2 0 3"
// into a workload for one process, P1, just large enough to hold every page.
func referenceString(refs string) ([]MemOp, error) {
	pages, err := parsePages(strings.FieldsFunc(refs, func(r rune) bool {
		return r == ' ' || r == ',' || r == '\t'
	}))
	if err != nil {
		return nil, err
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("-refs is empty")
	}
	highest := 0
	for _, p := range pages {
		if p < 0 {
			return nil, fmt.Errorf("page numbers cannot be negative, got %d", p)
		}
		highest = max(highest, p)
	}
	return []MemOp{
		{Kind: OpAlloc, PID: "P1", Count: highest + 1},
		{Kind: OpAccess, PID: "P1", Pages: pages},
	}, nil
}

// memRunner applies workload operations to a memory and reports them.
type memRunner struct {
	m     *Memory
	out   io.Writer
	trace bool // print a line for every operation and page reference

	overcommitted bool // the overcommit warning has been printed
}

// run applies every operation in order. A segmentation fault is part of the
// simulation and does not stop the run; any other error does.
func (r *memRunner) run(ops []MemOp) error {
	for _, op := range ops {
		if err := r.exec(op); err != nil {
			var segv *SegmentationFault
			if !errors.As(err, &segv) {
				return err
			}
		}
	}
	return nil
}

// exec applies one operation.
func (r *memRunner) exec(op MemOp) error {
	m := r.m
	switch op.Kind {
	case OpAlloc:
		if err := m.Allocate(op.PID, op.Count); err != nil {
			return err
		}
		r.logf("  %-12s %s allocated %d page(s), %s of virtual memory, none resident yet\n",
			"alloc", op.PID, op.Count, formatBytes(op.Count*m.PageSize))
		if demand := m.Demand(); demand > len(m.Frames) && !r.overcommitted {
			r.overcommitted = true
			r.logf("  %-12s %d page(s) allocated for %d frame(s): memory is overcommitted, page replacement will be needed\n",
				"overflow", demand, len(m.Frames))
		}
	case OpFree:
		released, err := m.Free(op.PID)
		if err != nil {
			return err
		}
		r.logf("  %-12s %s freed, %d frame(s) released, %d of %d now in use\n",
			"free", op.PID, released, m.Used(), len(m.Frames))
		if m.Demand() <= len(m.Frames) {
			r.overcommitted = false
		}
	case OpAccess:
		for _, page := range op.Pages {
			res, err := m.Access(op.PID, page)
			if err != nil {
				var segv *SegmentationFault
				if errors.As(err, &segv) {
					r.logf("  %-12s %s\n", "SEGFAULT", err)
				}
				return err
			}
			r.traceAccess(res)
		}
	case OpFrames:
		if r.trace {
			fmt.Fprintln(r.out)
			printFrames(r.out, m)
			fmt.Fprintln(r.out)
		}
	case OpStats:
		if r.trace {
			fmt.Fprintln(r.out)
			printMemReport(r.out, m)
			fmt.Fprintln(r.out)
		}
	}
	return nil
}

func (r *memRunner) logf(format string, args ...any) {
	if r.trace {
		fmt.Fprintf(r.out, format, args...)
	}
}

// traceAccess prints one page reference, followed by the frame contents when
// memory is small enough for them to fit on the line.
func (r *memRunner) traceAccess(res AccessResult) {
	if !r.trace {
		return
	}
	var what string
	switch {
	case !res.Fault:
		what = fmt.Sprintf("hit    in frame %d", res.Frame)
	case res.Victim == nil:
		what = fmt.Sprintf("FAULT  loaded into free frame %d", res.Frame)
	default:
		what = fmt.Sprintf("FAULT  memory full, %s replaces %s in frame %d", r.m.Policy, res.Victim, res.Frame)
	}
	ref := fmt.Sprintf("%s:%d", res.PID, res.Page)
	line := fmt.Sprintf("  t=%-4d %-7s %-50s", res.Time, ref, what)
	if len(r.m.Frames) <= 8 {
		line += " " + frameStrip(r.m)
	}
	fmt.Fprintln(r.out, strings.TrimRight(line, " "))
}

// frameStrip renders the frame contents as "[P1:0 P1:1 -   ]".
func frameStrip(m *Memory) string {
	cells := make([]string, len(m.Frames))
	for i, f := range m.Frames {
		if f.Used {
			cells[i] = fmt.Sprintf("%-5s", PageRef{f.PID, f.Page})
		} else {
			cells[i] = fmt.Sprintf("%-5s", "-")
		}
	}
	return "[" + strings.Join(cells, " ") + "]"
}

func printMemHeader(m *Memory) {
	fmt.Printf("Paging | %s page replacement | %d frame(s) x %s = %s of physical memory\n",
		m.Policy, len(m.Frames), formatBytes(m.PageSize), formatBytes(len(m.Frames)*m.PageSize))
}

// printFrames prints the memory map: what every frame holds.
func printFrames(out io.Writer, m *Memory) {
	fmt.Fprintf(out, "Memory map (%d of %d frame(s) in use, %s of %s)\n",
		m.Used(), len(m.Frames), formatBytes(m.Used()*m.PageSize), formatBytes(len(m.Frames)*m.PageSize))
	fmt.Fprintln(out, "Frame   Page      Loaded   Last used")
	for i, f := range m.Frames {
		if !f.Used {
			fmt.Fprintf(out, "%5d   %-8s\n", i, "free")
			continue
		}
		fmt.Fprintf(out, "%5d   %-8s %7s %11s\n", i, PageRef{f.PID, f.Page},
			"t="+strconv.Itoa(f.LoadedAt), "t="+strconv.Itoa(f.LastUsed))
	}
}

// printMemReport prints memory usage and page faults per process, then the totals.
func printMemReport(out io.Writer, m *Memory) {
	fmt.Fprintln(out, "Process   Pages   Virtual   Resident   Memory used   Peak used   Accesses   Hits   Faults   Fault rate   Evicted")
	for _, p := range m.Processes() {
		used := formatBytes(p.Resident * m.PageSize)
		if p.Freed {
			used = "freed"
		}
		fmt.Fprintf(out, "%-9s %5d %9s %10d %13s %11s %10d %6d %8d %11.2f%% %9d\n",
			p.PID, p.Pages(), formatBytes(p.Pages()*m.PageSize), p.Resident, used,
			formatBytes(p.PeakResident*m.PageSize), p.Accesses, p.Hits, p.Faults, p.FaultRate(), p.Evicted)
	}

	fmt.Fprintf(out, "\nPage references:  %d\n", m.Accesses)
	fmt.Fprintf(out, "Page faults:      %d (%.2f%%)\n", m.Faults, m.FaultRate())
	fmt.Fprintf(out, "Hits:             %d (%.2f%%)\n", m.Hits, m.HitRate())
	fmt.Fprintf(out, "Replacements:     %d (faults that found memory full)\n", m.Replacements)
	fmt.Fprintf(out, "Frames in use:    %d of %d now, peak %d (%s of %s)\n",
		m.Used(), len(m.Frames), m.PeakUsed, formatBytes(m.PeakUsed*m.PageSize), formatBytes(len(m.Frames)*m.PageSize))
	invalid := 0
	for _, p := range m.Processes() {
		invalid += p.Invalid
	}
	if invalid > 0 {
		fmt.Fprintf(out, "Segfaults:        %d (references outside an address space)\n", invalid)
	}
}

// printComparison sets the runs of the same workload side by side.
func printComparison(runs []*Memory) {
	fmt.Println("Comparison")
	fmt.Println("Algorithm   Faults   Fault rate   Hits   Replacements")
	for _, m := range runs {
		fmt.Printf("%-9s %8d %11.2f%% %6d %14d\n", m.Policy, m.Faults, m.FaultRate(), m.Hits, m.Replacements)
	}
	fmt.Println()
}

// printSweep runs the workload silently for every frame count from 1 to n
// under both algorithms. It is the classic way to see Belady's anomaly: FIFO
// can fault more with more frames, LRU never does.
func printSweep(workload []MemOp, n, pageSize int) error {
	fmt.Println("Page faults by number of frames")
	fmt.Println("Frames   FIFO    LRU")
	prevFIFO := -1
	for frames := 1; frames <= n; frames++ {
		var faults [2]int
		for i, policy := range []Replacement{ReplaceFIFO, ReplaceLRU} {
			m, err := NewMemory(frames, pageSize, policy)
			if err != nil {
				return err
			}
			if err := (&memRunner{m: m}).run(workload); err != nil {
				return err
			}
			faults[i] = m.Faults
		}
		note := ""
		if prevFIFO >= 0 && faults[0] > prevFIFO {
			note = "   <- Belady's anomaly: FIFO faults more with more frames"
		}
		fmt.Printf("%6d %6d %6d%s\n", frames, faults[0], faults[1], note)
		prevFIFO = faults[0]
	}
	fmt.Println()
	return nil
}

// memPrompt reads operations from in until EOF or "quit", applying each one as
// soon as it is typed. Mistakes are reported and the prompt carries on.
func memPrompt(m *Memory, in io.Reader, out io.Writer) {
	r := &memRunner{m: m, out: out, trace: true}
	fmt.Fprintln(out, "Type \"help\" for the operations, \"quit\" to leave.")
	scanner := bufio.NewScanner(in)
	for {
		fmt.Fprint(out, "mem> ")
		if !scanner.Scan() {
			fmt.Fprintln(out)
			break
		}
		text := strings.TrimSpace(scanner.Text())
		switch strings.ToLower(text) {
		case "", "#":
			continue
		case "quit", "exit":
			fmt.Fprintln(out)
			printMemReport(out, m)
			return
		case "help", "?":
			fmt.Fprintln(out, "  alloc <pid> <pages>      give a process a virtual address space")
			fmt.Fprintln(out, "  access <pid> <page>...   reference pages (or just \"<pid> <page>...\")")
			fmt.Fprintln(out, "  free <pid>               deallocate a process")
			fmt.Fprintln(out, "  frames                   show the memory map")
			fmt.Fprintln(out, "  stats                    show memory usage and page faults per process")
			fmt.Fprintln(out, "  quit                     print the summary and leave")
			continue
		}
		op, err := parseMemOp(text)
		if err == nil {
			err = r.exec(op)
		}
		var segv *SegmentationFault
		if err != nil && !errors.As(err, &segv) {
			fmt.Fprintf(out, "  error: %v\n", err)
		}
	}
	printMemReport(out, m)
}

// formatBytes renders a byte count as B, KB or MB.
func formatBytes(n int) string {
	switch {
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MB", n>>20)
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%d KB", n>>10)
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
