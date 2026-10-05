package main

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Text filters: the commands that sit in the middle of pipelines. They read
// standard input when no file is named, write standard output, and go through
// the permission-checked file system for any file they are given - which is
// why they are built in instead of being the host's grep and sort.

// parseShortFlags splits leading "-abc" style options from the operands.
// Options listed in withValue take the next argument (or the rest of the same
// one: "-n5"). A bare "-N" is reported as the value of numberFlag, so that
// "head -3" works.
func parseShortFlags(cmd string, args []string, known, withValue, numberFlag string) (map[rune]string, []string, error) {
	flags := map[rune]string{}
	i := 0
outer:
	for ; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			i++
			break
		}
		if len(a) < 2 || a[0] != '-' {
			break
		}
		if numberFlag != "" && isDigits(a[1:]) {
			flags[rune(numberFlag[0])] = a[1:]
			continue
		}
		for j, c := range a[1:] {
			switch {
			case strings.ContainsRune(withValue, c):
				v := a[2+j:]
				if v == "" {
					if i+1 >= len(args) {
						return nil, nil, fmt.Errorf("%s: option requires an argument -- '%c'", cmd, c)
					}
					i++
					v = args[i]
				}
				flags[c] = v
				continue outer
			case strings.ContainsRune(known, c):
				flags[c] = ""
			default:
				return nil, nil, fmt.Errorf("%s: invalid option -- '%c'", cmd, c)
			}
		}
	}
	return flags, args[i:], nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func has(flags map[rune]string, c rune) bool { _, ok := flags[c]; return ok }

// readLines reads every line of the inputs (files or standard input).
func readLines(sh *Shell, cmd string, operands []string, in io.Reader, errw io.Writer) ([]string, int) {
	var lines []string
	status := eachInput(sh, cmd, operands, in, errw, func(_ string, r io.Reader) {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
	})
	return lines, status
}

// builtinGrep prints the lines that match a regular expression.
//
//	-i ignore case   -v invert   -n line numbers   -c count only
func builtinGrep(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	flags, rest, err := parseShortFlags("grep", args[1:], "ivnc", "", "")
	if err != nil {
		fmt.Fprintln(errw, err)
		return 2
	}
	if len(rest) == 0 {
		fmt.Fprintln(errw, "grep: usage: grep [-ivnc] pattern [file ...]")
		return 2
	}
	pattern := rest[0]
	if has(flags, 'i') {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		fmt.Fprintf(errw, "grep: invalid pattern: %v\n", err)
		return 2
	}

	files := rest[1:]
	matched := false
	status := eachInput(sh, "grep", files, in, errw, func(name string, r io.Reader) {
		prefix := ""
		if len(files) > 1 {
			prefix = name + ":"
		}
		count, lineNo := 0, 0
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			lineNo++
			if re.MatchString(sc.Text()) == has(flags, 'v') {
				continue
			}
			matched = true
			count++
			if has(flags, 'c') {
				continue
			}
			if has(flags, 'n') {
				fmt.Fprintf(out, "%s%d:%s\n", prefix, lineNo, sc.Text())
			} else {
				fmt.Fprintf(out, "%s%s\n", prefix, sc.Text())
			}
		}
		if has(flags, 'c') {
			fmt.Fprintf(out, "%s%d\n", prefix, count)
		}
	})
	switch {
	case status != 0:
		return 2
	case !matched:
		return 1 // grep's way of saying "no match", handy in scripts
	}
	return 0
}

// builtinSort sorts lines.
//
//	-r reverse   -n numeric (by leading number)   -u drop duplicates
func builtinSort(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	flags, files, err := parseShortFlags("sort", args[1:], "rnu", "", "")
	if err != nil {
		fmt.Fprintln(errw, err)
		return 2
	}
	lines, status := readLines(sh, "sort", files, in, errw)

	less := func(a, b string) bool { return a < b }
	if has(flags, 'n') {
		less = func(a, b string) bool {
			x, y := leadingNumber(a), leadingNumber(b)
			if x != y {
				return x < y
			}
			return a < b
		}
	}
	sort.SliceStable(lines, func(i, j int) bool {
		if has(flags, 'r') {
			return less(lines[j], lines[i])
		}
		return less(lines[i], lines[j])
	})

	for i, l := range lines {
		if has(flags, 'u') && i > 0 && l == lines[i-1] {
			continue
		}
		fmt.Fprintln(out, l)
	}
	return status
}

// leadingNumber is the number a line starts with (after blanks), or 0.
func leadingNumber(s string) float64 {
	s = strings.TrimLeftFunc(s, unicode.IsSpace)
	end := 0
	for end < len(s) && (s[end] == '-' && end == 0 || s[end] == '.' || s[end] >= '0' && s[end] <= '9') {
		end++
	}
	f, _ := strconv.ParseFloat(s[:end], 64)
	return f
}

// builtinUniq collapses runs of identical adjacent lines; -c prefixes each with
// how many there were, -d prints only the repeated ones.
func builtinUniq(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	flags, files, err := parseShortFlags("uniq", args[1:], "cd", "", "")
	if err != nil {
		fmt.Fprintln(errw, err)
		return 2
	}
	if len(files) > 1 {
		fmt.Fprintln(errw, "uniq: usage: uniq [-cd] [file]")
		return 2
	}
	lines, status := readLines(sh, "uniq", files, in, errw)
	emit := func(line string, n int) {
		if has(flags, 'd') && n < 2 {
			return
		}
		if has(flags, 'c') {
			fmt.Fprintf(out, "%7d %s\n", n, line)
		} else {
			fmt.Fprintln(out, line)
		}
	}
	for i := 0; i < len(lines); {
		j := i
		for j < len(lines) && lines[j] == lines[i] {
			j++
		}
		emit(lines[i], j-i)
		i = j
	}
	return status
}

// builtinWc counts lines, words and bytes.
func builtinWc(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	flags, files, err := parseShortFlags("wc", args[1:], "lwc", "", "")
	if err != nil {
		fmt.Fprintln(errw, err)
		return 1
	}
	if len(flags) == 0 {
		flags = map[rune]string{'l': "", 'w': "", 'c': ""}
	}
	var total [3]int
	print := func(counts [3]int, name string) {
		var b strings.Builder
		for i, c := range []rune{'l', 'w', 'c'} {
			if has(flags, c) {
				fmt.Fprintf(&b, "%8d", counts[i])
			}
		}
		if name != "" {
			b.WriteString(" " + name)
		}
		fmt.Fprintln(out, b.String())
	}
	status := eachInput(sh, "wc", files, in, errw, func(name string, r io.Reader) {
		var counts [3]int
		br := bufio.NewReader(r)
		inWord := false
		for {
			c, size, err := br.ReadRune()
			if err != nil {
				break
			}
			counts[2] += size
			if c == '\n' {
				counts[0]++
			}
			if unicode.IsSpace(c) {
				inWord = false
			} else if !inWord {
				inWord = true
				counts[1]++
			}
		}
		for i := range total {
			total[i] += counts[i]
		}
		print(counts, name)
	})
	if len(files) > 1 {
		print(total, "total")
	}
	return status
}

// builtinHead prints the first lines of its input. It stops reading as soon as
// it has them: when it is the end of a pipeline, closing its end of the pipe
// makes the writer upstream (yes, cat of a huge file, ...) stop with SIGPIPE.
func builtinHead(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	return headTail(sh, "head", args, in, out, errw)
}

// builtinTail prints the last lines of its input.
func builtinTail(sh *Shell, args []string, in io.Reader, out, errw io.Writer) int {
	return headTail(sh, "tail", args, in, out, errw)
}

func headTail(sh *Shell, cmd string, args []string, in io.Reader, out, errw io.Writer) int {
	flags, files, err := parseShortFlags(cmd, args[1:], "", "n", "n")
	if err != nil {
		fmt.Fprintln(errw, err)
		return 1
	}
	n := 10
	if v, ok := flags['n']; ok {
		if n, err = strconv.Atoi(v); err != nil || n < 0 {
			fmt.Fprintf(errw, "%s: %s: invalid number of lines\n", cmd, v)
			return 1
		}
	}
	return eachInput(sh, cmd, files, in, errw, func(name string, r io.Reader) {
		if len(files) > 1 {
			fmt.Fprintf(out, "==> %s <==\n", name)
		}
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		if cmd == "head" {
			for i := 0; i < n && sc.Scan(); i++ {
				fmt.Fprintln(out, sc.Text())
			}
			return
		}
		var ring []string
		for sc.Scan() {
			ring = append(ring, sc.Text())
			if len(ring) > n {
				ring = ring[1:]
			}
		}
		for _, l := range ring {
			fmt.Fprintln(out, l)
		}
	})
}
