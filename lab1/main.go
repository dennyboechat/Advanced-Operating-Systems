// Lab 1 - Concurrent word frequency counter.
//
// The input file is partitioned into N segments. Each segment is processed by
// its own goroutine (thread), which computes and reports the word frequency of
// its segment into an intermediate data structure. The main process waits for
// every worker to finish and then consolidates the intermediate counts into the
// final word frequency.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"unicode"
)

// SegmentResult is the intermediate data structure produced by a single worker.
type SegmentResult struct {
	ID     int            // segment / thread number
	Start  int            // byte offset of the segment start (inclusive)
	End    int            // byte offset of the segment end (exclusive)
	Counts map[string]int // word frequency for this segment only
}

func main() {
	file := flag.String("file", "", "path to the input text file (required)")
	n := flag.Int("n", 1, "number of segments/threads (N)")
	top := flag.Int("top", 0, "print only the top K words of the final result (0 = all)")
	flag.Parse()

	if *file == "" {
		fmt.Fprintln(os.Stderr, "error: -file is required")
		flag.Usage()
		os.Exit(1)
	}
	if *n < 1 {
		fmt.Fprintln(os.Stderr, "error: -n must be at least 1")
		os.Exit(1)
	}

	data, err := os.ReadFile(*file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot read %s: %v\n", *file, err)
		os.Exit(1)
	}

	bounds := partition(data, *n)

	// Intermediate storage: one slot per thread, so no mutex is needed while
	// the workers run - each goroutine owns exactly one element of the slice.
	results := make([]SegmentResult, len(bounds))

	var wg sync.WaitGroup
	for i, b := range bounds {
		wg.Add(1)
		go func(id int, start, end int) {
			defer wg.Done()
			results[id] = SegmentResult{
				ID:     id,
				Start:  start,
				End:    end,
				Counts: countWords(string(data[start:end])),
			}
		}(i, b.start, b.end)
	}

	// The main process waits until all threads have completed execution.
	wg.Wait()

	fmt.Printf("File: %s (%d bytes)\n", *file, len(data))
	fmt.Printf("Segments requested: %d, segments created: %d\n\n", *n, len(bounds))

	// Intermediate output, one block per thread.
	for _, r := range results {
		fmt.Printf("--- Thread %d | bytes [%d, %d) | %d distinct words ---\n",
			r.ID, r.Start, r.End, len(r.Counts))
		printCounts(r.Counts, 0)
		fmt.Println()
	}

	// Consolidation performed by the main process after all threads finish.
	final := make(map[string]int)
	for _, r := range results {
		for word, count := range r.Counts {
			final[word] += count
		}
	}

	total := 0
	for _, c := range final {
		total += c
	}

	fmt.Printf("=== Final consolidated word frequency | %d distinct words, %d total words ===\n",
		len(final), total)
	printCounts(final, *top)
}

type bound struct{ start, end int }

// partition splits data into at most n byte ranges, moving each boundary
// forward to the next whitespace so that no word is cut in half.
func partition(data []byte, n int) []bound {
	if len(data) == 0 {
		return []bound{{0, 0}}
	}
	if n > len(data) {
		n = len(data)
	}

	size := len(data) / n
	bounds := make([]bound, 0, n)
	start := 0

	for i := 0; i < n && start < len(data); i++ {
		end := len(data)
		if i < n-1 {
			end = start + size
			// Extend to the end of the current word.
			for end < len(data) && !isSeparator(rune(data[end])) {
				end++
			}
			if end > len(data) {
				end = len(data)
			}
		}
		bounds = append(bounds, bound{start, end})
		start = end
	}

	// Any remainder caused by boundary adjustment belongs to the last segment.
	if start < len(data) {
		bounds[len(bounds)-1].end = len(data)
	}
	return bounds
}

// countWords normalizes text to lowercase words and returns their frequency.
func countWords(text string) map[string]int {
	counts := make(map[string]int)
	for _, field := range strings.FieldsFunc(text, isSeparator) {
		word := strings.Trim(strings.ToLower(field), "'")
		if word != "" {
			counts[word]++
		}
	}
	return counts
}

// isSeparator reports whether r separates two words. Letters, digits and the
// apostrophe (for contractions such as "don't") are part of a word.
func isSeparator(r rune) bool {
	if r == '\'' {
		return false
	}
	return !unicode.IsLetter(r) && !unicode.IsDigit(r)
}

// printCounts writes counts sorted by descending frequency, then alphabetically.
// When limit > 0 only the first limit entries are printed.
func printCounts(counts map[string]int, limit int) {
	words := make([]string, 0, len(counts))
	for w := range counts {
		words = append(words, w)
	}
	sort.Slice(words, func(i, j int) bool {
		if counts[words[i]] != counts[words[j]] {
			return counts[words[i]] > counts[words[j]]
		}
		return words[i] < words[j]
	})

	if limit > 0 && limit < len(words) {
		words = words[:limit]
	}
	for _, w := range words {
		fmt.Printf("  %-20s %d\n", w, counts[w])
	}
	if len(words) == 0 {
		fmt.Println("  (no words)")
	}
}
