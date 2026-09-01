# Lab 1 — Concurrent Word Frequency Counter (Go)

Counts word frequencies in a text file. The file is split into **N segments**, each
processed by its own thread. Every thread prints its intermediate counts; the main
process waits for all threads, then consolidates them into the final result.

## Requirements

Go 1.21+ (developed on 1.24).

## Run

```
go run . -file sample.txt -n 4
```

Or build first:

```
go build -o lab1 .
./lab1 -file sample.txt -n 4
```

| Flag    | Description                                      | Default |
|---------|--------------------------------------------------|---------|
| `-file` | Path to the input text file (**required**)       | —       |
| `-n`    | Number of segments / threads                     | `1`     |
| `-top`  | Show only the top K words in the final result    | `0` (all) |

## How it works

1. Read the file, then split its bytes into N segments — each boundary is pushed
   forward to the next non-word character so no word is split.
2. Launch one goroutine per segment; each builds its own `map[string]int`.
3. Store results in `[]SegmentResult`, one slot per thread (no mutex needed —
   each goroutine writes only its own index).
4. Main blocks on `sync.WaitGroup.Wait()` until all threads finish.
5. Main merges the intermediate maps and prints the final counts, sorted by
   frequency then alphabetically.

Words are lowercased; letters, digits and apostrophes (`don't`) count as word
characters. If `-n` exceeds the file size, fewer segments are created.

## Output

```
File: sample.txt (501 bytes)
Segments requested: 3, segments created: 3

--- Thread 0 | bytes [0, 174) | 16 distinct words ---
  the                  6
  dog                  4
  ...

--- Thread 1 | bytes [174, 344) | 22 distinct words ---
  the                  3
  threads              3
  ...

=== Final consolidated word frequency | 47 distinct words, 89 total words ===
  the                  10
  dog                  4
  ...
```

## Files

| File         | Purpose                        |
|--------------|--------------------------------|
| `main.go`    | Entire program                 |
| `sample.txt` | Sample input (89 words)        |
| `go.mod`     | Module definition              |
