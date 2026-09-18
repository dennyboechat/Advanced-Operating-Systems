// Deliverable 2 - CPU scheduling algorithms.
//
// Round robin: each process is given a time slice (quantum) chosen by the user
// on the command line. When the slice expires the scheduler switches to the
// next process in the ready queue; a process that finishes before its slice is
// over is removed from the queue and the next process is scheduled immediately.
//
// Priority (preemptive): the most urgent ready process always holds the CPU,
// equal priorities are served first come, first served, and the arrival of a
// higher-priority process preempts the running one. The ready queue is a heap.
//
// Both can be run instantly (a discrete-time simulation) or with -live, which
// executes the run in real time: every process is a goroutine that sleeps for
// the time it is given, and timers drive the switches.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func main() {
	algo := flag.String("algo", "rr", "scheduling algorithm: \"rr\" or \"priority\"")
	var quantum int
	flag.IntVar(&quantum, "quantum", 2, "round robin: time slice in time units (must be >= 1)")
	flag.IntVar(&quantum, "q", 2, "shorthand for -quantum")
	priority := flag.String("priority", "low", "priority: which number wins, \"low\" or \"high\"")
	contextSwitch := flag.Int("cs", 0, "context-switch overhead in time units")
	live := flag.Bool("live", false, "execute the run in real time with timers instead of simulating it instantly")
	tick := flag.Duration("tick", 100*time.Millisecond, "live: real duration of one time unit")
	file := flag.String("file", "", "process file: one \"name arrival burst [priority]\" per line")
	list := flag.String("procs", "", "inline processes, e.g. \"P1:0:5:2,P2:1:3:0\"")
	quiet := flag.Bool("quiet", false, "print only the Gantt chart and the summary table")

	flag.Usage = usage
	flag.Parse()

	procs, source, err := loadProcesses(*file, *list)
	if err != nil {
		fail(err)
	}

	rule, err := ParsePriorityRule(*priority)
	if err != nil {
		fail(err)
	}
	policy, err := ParsePolicy(*algo)
	if err != nil {
		fail(err)
	}
	byPriority := policy == PolicyPriority

	if byPriority {
		fmt.Printf("Priority scheduling (preemptive) | %s, ties served FCFS", rule)
	} else {
		fmt.Printf("Round-robin scheduling | time slice = %d", quantum)
	}
	if *contextSwitch > 0 {
		fmt.Printf(", context switch = %d", *contextSwitch)
	}
	if *live {
		fmt.Printf(", live: 1 time unit = %v", *tick)
	}
	fmt.Printf("\nProcesses: %d (%s)\n\n", len(procs), source)
	printProcessTable(procs, byPriority)

	var result Result
	switch {
	case *live:
		cfg := LiveConfig{
			Policy:        policy,
			Quantum:       quantum,
			Rule:          rule,
			ContextSwitch: *contextSwitch,
			Tick:          *tick,
		}
		if !*quiet {
			fmt.Println("Live execution")
			cfg.Log = liveLogger()
		}
		result, err = RunLive(procs, cfg)
		if cfg.Log != nil {
			fmt.Println()
		}
	case byPriority:
		result, err = PriorityScheduling(procs, rule, *contextSwitch)
	default:
		result, err = RoundRobin(procs, quantum, *contextSwitch)
	}
	if err != nil {
		fail(err)
	}

	// A live run already reported every event as it happened.
	if !*quiet && !result.Live {
		printTrace(result)
	}
	printGantt(result)
	printStats(result, byPriority)
}

// liveLogger returns a logging function that stamps each event with the real
// time elapsed since the first call.
func liveLogger() func(string, ...any) {
	start := time.Now()
	return func(format string, args ...any) {
		fmt.Printf("  [%7.2fs] %s\n", time.Since(start).Seconds(), fmt.Sprintf(format, args...))
	}
}

func usage() {
	out := flag.CommandLine.Output()
	name := filepath.Base(os.Args[0])
	fmt.Fprintf(out, "Usage: %s [flags]\n\n", name)
	fmt.Fprintln(out, "Simulates CPU scheduling: round robin with a user-supplied time slice, or")
	fmt.Fprintln(out, "preemptive priority scheduling with a heap-based ready queue.")
	fmt.Fprintln(out, "Processes come from -file or -procs; a built-in sample set is used if neither is given.")
	fmt.Fprintln(out, "\nFlags:")
	flag.PrintDefaults()
	fmt.Fprintln(out, "\nExamples:")
	fmt.Fprintf(out, "  %s -algo rr -q 3 -file processes.txt\n", name)
	fmt.Fprintf(out, "  %s -algo priority -file processes.txt\n", name)
	fmt.Fprintf(out, "  %s -algo priority -priority high -procs \"P1:0:5:1,P2:1:3:9\"\n", name)
	fmt.Fprintf(out, "  %s -algo priority -live -tick 200ms -file processes.txt\n", name)
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}

// defaultProcesses is used when the user supplies neither -file nor -procs.
var defaultProcesses = []Process{
	{"P1", 0, 5, 2},
	{"P2", 1, 4, 1},
	{"P3", 2, 7, 3},
	{"P4", 4, 3, 0},
}

// loadProcesses reads the process set from a file, from the inline list, or
// falls back to the built-in sample, and reports where it came from.
func loadProcesses(file, list string) ([]Process, string, error) {
	switch {
	case file != "" && list != "":
		return nil, "", fmt.Errorf("use either -file or -procs, not both")
	case file != "":
		procs, err := parseFile(file)
		return procs, "from " + file, err
	case list != "":
		procs, err := parseList(list)
		return procs, "from -procs", err
	default:
		return defaultProcesses, "built-in sample set", nil
	}
}

// parseFile reads "name arrival burst [priority]" records, one per line. Blank
// lines and lines starting with # are ignored; fields may be separated by
// spaces, commas or tabs.
func parseFile(path string) ([]Process, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var procs []Process
	scanner := bufio.NewScanner(f)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.FieldsFunc(text, func(r rune) bool {
			return r == ' ' || r == '\t' || r == ',' || r == ';'
		})
		p, err := parseFields(fields)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %v", path, line, err)
		}
		procs = append(procs, p)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(procs) == 0 {
		return nil, fmt.Errorf("%s contains no processes", path)
	}
	return procs, nil
}

// parseList parses the inline form "P1:0:5,P2:1:3" or "P1:0:5:2,P2:1:3:0".
func parseList(list string) ([]Process, error) {
	var procs []Process
	for _, entry := range strings.Split(list, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		p, err := parseFields(strings.Split(entry, ":"))
		if err != nil {
			return nil, fmt.Errorf("%q: %v", entry, err)
		}
		procs = append(procs, p)
	}
	if len(procs) == 0 {
		return nil, fmt.Errorf("-procs is empty")
	}
	return procs, nil
}

// parseFields turns [name, arrival, burst] or [name, arrival, burst, priority]
// into a Process. The priority defaults to 0 when it is left out.
func parseFields(fields []string) (Process, error) {
	if len(fields) != 3 && len(fields) != 4 {
		return Process{}, fmt.Errorf("expected \"name arrival burst [priority]\", got %d field(s)", len(fields))
	}
	arrival, err := strconv.Atoi(strings.TrimSpace(fields[1]))
	if err != nil {
		return Process{}, fmt.Errorf("arrival time %q is not a number", fields[1])
	}
	burst, err := strconv.Atoi(strings.TrimSpace(fields[2]))
	if err != nil {
		return Process{}, fmt.Errorf("burst time %q is not a number", fields[2])
	}
	priority := 0
	if len(fields) == 4 {
		if priority, err = strconv.Atoi(strings.TrimSpace(fields[3])); err != nil {
			return Process{}, fmt.Errorf("priority %q is not a number", fields[3])
		}
	}
	return Process{
		Name:     strings.TrimSpace(fields[0]),
		Arrival:  arrival,
		Burst:    burst,
		Priority: priority,
	}, nil
}

func printProcessTable(procs []Process, showPriority bool) {
	fmt.Println("Process   Arrival   Burst" + column(showPriority, "   Priority"))
	for _, p := range procs {
		fmt.Printf("%-9s %7d %7d", p.Name, p.Arrival, p.Burst)
		if showPriority {
			fmt.Printf(" %10d", p.Priority)
		}
		fmt.Println()
	}
	fmt.Println()
}

// printTrace writes one line per scheduling event, in simulated time order.
func printTrace(r Result) {
	fmt.Println("Execution trace")
	for _, s := range r.Timeline {
		switch s.Kind {
		case KindIdle:
			fmt.Printf("  t=%-4d CPU idle for %d unit(s), ready queue empty\n", s.Start, s.Duration())
		case KindSwitch:
			fmt.Printf("  t=%-4d context switch (%d unit(s))\n", s.Start, s.Duration())
		case KindRun:
			fmt.Printf("  t=%-4d %-4s runs %d unit(s) -> t=%d, ", s.Start, s.Name, s.Duration(), s.End)
			switch {
			case s.Completed:
				fmt.Printf("finished, removed from the queue\n")
			case s.Preemptor != "":
				fmt.Printf("preempted by %s, %d unit(s) left, requeued\n", s.Preemptor, s.Remaining)
			default:
				fmt.Printf("slice expired, %d unit(s) left, requeued\n", s.Remaining)
			}
		}
	}
	fmt.Println()
}

// printGantt draws the timeline as an ASCII Gantt chart.
func printGantt(r Result) {
	fmt.Println("Gantt chart")

	labels := make([]string, len(r.Timeline))
	widths := make([]int, len(r.Timeline))
	for i, s := range r.Timeline {
		switch s.Kind {
		case KindIdle:
			labels[i] = "idle"
		case KindSwitch:
			labels[i] = "cs"
		default:
			labels[i] = s.Name
		}
		widths[i] = max(len(labels[i])+2, len(strconv.Itoa(s.Start))+1)
	}

	var bar, marks strings.Builder
	bar.WriteString("  |")
	marks.WriteString("  ")
	for i, s := range r.Timeline {
		bar.WriteString(center(labels[i], widths[i]) + "|")
		marks.WriteString(pad(strconv.Itoa(s.Start), widths[i]+1))
	}
	if len(r.Timeline) > 0 {
		marks.WriteString(strconv.Itoa(r.Makespan))
	}

	fmt.Println(bar.String())
	fmt.Println(marks.String())
	fmt.Println()
}

// printStats writes the per-process results followed by the run averages.
func printStats(r Result, showPriority bool) {
	fmt.Println("Process   Arrival   Burst" + column(showPriority, "   Priority") +
		"   Completion   Turnaround   Waiting   Response   Preemptions")
	for _, s := range r.Stats {
		fmt.Printf("%-9s %7d %7d", s.Name, s.Arrival, s.Burst)
		if showPriority {
			fmt.Printf(" %10d", s.Priority)
		}
		fmt.Printf(" %12d %12d %9d %10d %13d\n",
			s.Completion, s.Turnaround, s.Waiting, s.Response, s.Preemptions)
	}

	fmt.Printf("\nAverage turnaround time: %.2f\n", r.AvgTurnaround())
	fmt.Printf("Average waiting time:    %.2f\n", r.AvgWaiting())
	fmt.Printf("Average response time:   %.2f\n", r.AvgResponse())
	fmt.Printf("Total time:              %d (busy %d, idle %d, context switches %d)\n",
		r.Makespan, r.BusyTime, r.IdleTime, r.SwitchTime)
	if r.Live {
		fmt.Printf("Real time elapsed:       %v (1 time unit = %v)\n", r.Wall.Round(time.Millisecond), r.Tick)
	}
	fmt.Printf("CPU utilization:         %.2f%%\n", r.Utilization())
	fmt.Printf("Throughput:              %.4f process/time unit\n", r.Throughput())
}

// column returns header when shown, so optional columns stay aligned.
func column(shown bool, header string) string {
	if shown {
		return header
	}
	return ""
}

// center pads s with spaces so that it is centred in a field of width w.
func center(s string, w int) string {
	if len(s) >= w {
		return s
	}
	left := (w - len(s)) / 2
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", w-len(s)-left)
}

// pad left-aligns s in a field of width w.
func pad(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}
