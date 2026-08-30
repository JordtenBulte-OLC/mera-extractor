// internal/logrotate/logrotate_test.go
package logrotate

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixedClock returns a now func whose value the test can move.
func fixedClock(t *time.Time) func() time.Time {
	return func() time.Time { return *t }
}

func mustNew(t *testing.T, path string, maxBytes int64) *Writer {
	t.Helper()
	w, err := New(path, maxBytes)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

func listLogs(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	return string(b)
}

// 1. A plain write lands in extractor-<today>.log and nothing else.
func TestBasicWrite(t *testing.T) {
	dir := t.TempDir()
	clk := time.Date(2026, 8, 30, 10, 0, 0, 0, time.Local)
	w := mustNew(t, filepath.Join(dir, "extractor.log"), 10<<20)
	w.now = fixedClock(&clk)

	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got := listLogs(t, dir)
	want := []string{"extractor-2026-08-30.log"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("files = %v, want %v", got, want)
	}
	if c := readFile(t, filepath.Join(dir, want[0])); c != "hello\n" {
		t.Fatalf("content = %q", c)
	}
}

// 2. A write that would cross the cap goes whole into the next iterator file.
func TestSizeRollDoesNotSplitRecords(t *testing.T) {
	dir := t.TempDir()
	clk := time.Date(2026, 8, 30, 10, 0, 0, 0, time.Local)
	w := mustNew(t, filepath.Join(dir, "extractor.log"), 20)
	w.now = fixedClock(&clk)

	a := []byte("0123456789\n") // 11 bytes
	b := []byte("abcdefghij\n")  // 11 bytes; 11+11 = 22 > 20 -> roll before b

	if _, err := w.Write(a); err != nil {
		t.Fatalf("Write a: %v", err)
	}
	if _, err := w.Write(b); err != nil {
		t.Fatalf("Write b: %v", err)
	}

	if c := readFile(t, filepath.Join(dir, "extractor-2026-08-30.log")); c != string(a) {
		t.Fatalf("file 0 = %q, want %q", c, a)
	}
	if c := readFile(t, filepath.Join(dir, "extractor-2026-08-30.1.log")); c != string(b) {
		t.Fatalf("file 1 = %q, want %q", c, b)
	}
}

// 3. Advancing the clock past midnight starts a fresh seq-0 file for the new day.
func TestDayRollResetsIterator(t *testing.T) {
	dir := t.TempDir()
	clk := time.Date(2026, 8, 30, 23, 59, 0, 0, time.Local)
	w := mustNew(t, filepath.Join(dir, "extractor.log"), 5) // tiny cap to force a .1 on day one
	w.now = fixedClock(&clk)

	_, _ = w.Write([]byte("aaaaaa\n"))
	_, _ = w.Write([]byte("bbbbbb\n")) // rolls to .1 on 08-30

	clk = time.Date(2026, 8, 31, 0, 0, 1, 0, time.Local)
	_, _ = w.Write([]byte("cccccc\n"))

	got := listLogs(t, dir)
	want := []string{
		"extractor-2026-08-30.1.log",
		"extractor-2026-08-30.log",
		"extractor-2026-08-31.log",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("files = %v, want %v", got, want)
	}
	if w.curSeq != 0 {
		t.Fatalf("curSeq after day roll = %d, want 0", w.curSeq)
	}
}

// 4. Restart mid-day with the existing file under the cap: append, don't roll.
func TestRestartAppendsUnderCap(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "extractor-2026-08-30.log")
	if err := os.WriteFile(name, []byte(strings.Repeat("x", 4000)), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	clk := time.Date(2026, 8, 30, 12, 0, 0, 0, time.Local)
	w := mustNew(t, filepath.Join(dir, "extractor.log"), 10<<20)
	w.now = fixedClock(&clk)
	_, _ = w.Write([]byte("more\n"))

	got := listLogs(t, dir)
	if len(got) != 1 || got[0] != "extractor-2026-08-30.log" {
		t.Fatalf("files = %v, want single seed file", got)
	}
	if c := readFile(t, name); !strings.HasSuffix(c, "more\n") || len(c) != 4005 {
		t.Fatalf("len = %d, suffix ok = %v", len(c), strings.HasSuffix(c, "more\n"))
	}
}

// 5. Restart mid-day with the existing file already at the cap: next write rolls.
func TestRestartOverCapRolls(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "extractor-2026-08-30.log")
	if err := os.WriteFile(name, []byte(strings.Repeat("x", 100)), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	clk := time.Date(2026, 8, 30, 12, 0, 0, 0, time.Local)
	w := mustNew(t, filepath.Join(dir, "extractor.log"), 50)
	w.now = fixedClock(&clk)
	_, _ = w.Write([]byte("y\n"))

	if c := readFile(t, filepath.Join(dir, "extractor-2026-08-30.1.log")); c != "y\n" {
		t.Fatalf("rolled file = %q, want %q", c, "y\n")
	}
}

// 6. A single record larger than the cap is written whole; the next write rolls.
func TestOversizedRecord(t *testing.T) {
	dir := t.TempDir()
	clk := time.Date(2026, 8, 30, 10, 0, 0, 0, time.Local)
	w := mustNew(t, filepath.Join(dir, "extractor.log"), 16)
	w.now = fixedClock(&clk)

	big := []byte(strings.Repeat("Z", 40) + "\n")
	if n, err := w.Write(big); err != nil || n != len(big) {
		t.Fatalf("Write big: n=%d err=%v", n, err)
	}
	if _, err := w.Write([]byte("next\n")); err != nil {
		t.Fatalf("Write next: %v", err)
	}

	if c := readFile(t, filepath.Join(dir, "extractor-2026-08-30.log")); c != string(big) {
		t.Fatalf("file 0 mismatch")
	}
	if c := readFile(t, filepath.Join(dir, "extractor-2026-08-30.1.log")); c != "next\n" {
		t.Fatalf("file 1 = %q", c)
	}
}

// 7. maxBytes <= 0 disables size rotation; only the day boundary rotates.
func TestNoSizeCap(t *testing.T) {
	dir := t.TempDir()
	clk := time.Date(2026, 8, 30, 10, 0, 0, 0, time.Local)
	w := mustNew(t, filepath.Join(dir, "extractor.log"), 0)
	w.now = fixedClock(&clk)

	for i := 0; i < 1000; i++ {
		if _, err := w.Write([]byte(strings.Repeat("q", 100) + "\n")); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	got := listLogs(t, dir)
	if len(got) != 1 {
		t.Fatalf("files = %v, want exactly one", got)
	}
}

// 8. Repeated size rolls produce .1 .2 .3 and the parser reads multi-digit
//    iterators correctly (".2" is 2, not confused with anything else).
func TestSequenceNumbering(t *testing.T) {
	dir := t.TempDir()
	clk := time.Date(2026, 8, 30, 10, 0, 0, 0, time.Local)
	w := mustNew(t, filepath.Join(dir, "extractor.log"), 10)
	w.now = fixedClock(&clk)

	for i := 0; i < 4; i++ {
		if _, err := w.Write([]byte("record\n")); err != nil { // 7 bytes; 14 > 10 -> roll every time
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	got := listLogs(t, dir)
	want := []string{
		"extractor-2026-08-30.1.log",
		"extractor-2026-08-30.2.log",
		"extractor-2026-08-30.3.log",
		"extractor-2026-08-30.log",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("files = %v, want %v", got, want)
	}

	// parseIterator sanity for a two-digit value.
	if n, ok := parseIterator(".12"); !ok || n != 12 {
		t.Fatalf("parseIterator(.12) = %d,%v", n, ok)
	}
}

// 9. A base name with no extension still rotates, with the iterator appended raw.
func TestNoExtension(t *testing.T) {
	dir := t.TempDir()
	clk := time.Date(2026, 8, 30, 10, 0, 0, 0, time.Local)
	w := mustNew(t, filepath.Join(dir, "extractor"), 8)
	w.now = fixedClock(&clk)

	_, _ = w.Write([]byte("aaaa\n"))
	_, _ = w.Write([]byte("bbbb\n"))

	got := listLogs(t, dir)
	want := []string{"extractor-2026-08-30", "extractor-2026-08-30.1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("files = %v, want %v", got, want)
	}
}

// 10. Concurrent writers: every line lands exactly once, no interleaving,
//     file count is consistent with the byte total and the cap.
func TestConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	clk := time.Date(2026, 8, 30, 10, 0, 0, 0, time.Local)
	const cap = 1 << 12
	w := mustNew(t, filepath.Join(dir, "extractor.log"), cap)
	w.now = fixedClock(&clk)

	const goroutines, perG = 16, 200
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				line := fmt.Sprintf("g%02d-i%03d-payload\n", g, i)
				if _, err := w.Write([]byte(line)); err != nil {
					t.Errorf("Write: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	seen := map[string]int{}
	var total int
	for _, name := range listLogs(t, dir) {
		content := readFile(t, filepath.Join(dir, name))
		total += len(content)
		for _, line := range strings.Split(strings.TrimSuffix(content, "\n"), "\n") {
			if line == "" {
				t.Fatalf("empty line in %s -> a record was split", name)
			}
			seen[line]++
		}
	}
	if len(seen) != goroutines*perG {
		t.Fatalf("distinct lines = %d, want %d", len(seen), goroutines*perG)
	}
	for line, c := range seen {
		if c != 1 {
			t.Fatalf("line %q written %d times", line, c)
		}
	}
	// Each file bar the last should be within one record of the cap.
	files := listLogs(t, dir)
	for _, name := range files[:len(files)-1] {
		if sz := len(readFile(t, filepath.Join(dir, name))); sz > cap+64 {
			t.Fatalf("%s size %d overshoots cap %d by more than a record", name, sz, cap)
		}
	}
}

// 11. highestSeqFor ignores other days, compressed rolls, and unrelated files.
func TestHighestSeqForIsolation(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{
		"extractor-2026-08-29.log",     // other day
		"extractor-2026-08-30.log.gz",  // compressed, wrong suffix
		"unrelated.log",                // not ours
		"extractor-2026-08-300.log",    // prefix-adjacent, must not match
	} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", n, err)
		}
	}
	w := mustNew(t, filepath.Join(dir, "extractor.log"), 10<<20)

	seq, size, err := w.highestSeqFor("2026-08-30")
	if err != nil {
		t.Fatalf("highestSeqFor: %v", err)
	}
	if seq != 0 || size != 0 {
		t.Fatalf("seq,size = %d,%d, want 0,0 (nothing valid for that day)", seq, size)
	}
}

// 12. New fails when the target directory cannot be created, so main.go can
//     fall back to stdout-only logging.
func TestNewMkdirFailure(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(parent, 0o500); err != nil { // read+execute, no write
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	if _, err := New(filepath.Join(parent, "sub", "extractor.log"), 1<<20); err == nil {
		t.Fatal("New succeeded, want error creating unwritable dir")
	}
}
