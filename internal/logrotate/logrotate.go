// internal/logrotate/logrotate.go
//
// Package logrotate provides a single io.Writer that rotates the file it
// writes to on two independent triggers:
//
//  1. The local calendar date changes  -> a fresh file for the new day.
//  2. The current file would exceed maxBytes -> a fresh file for the same
//     day, distinguished by a numeric iterator.
//
// File names carry the date, and the iterator only once more than one file
// exists for that day:
//
//	extractor-2026-08-30.log      (first file of the day, iterator 0)
//	extractor-2026-08-30.1.log    (rolled once on size)
//	extractor-2026-08-30.2.log    (rolled again)
//
// ▶ Why a bespoke writer and not gopkg.in/natefinch/lumberjack.v2:
// lumberjack rotates on size (or an explicit Rotate call) but has no notion
// of a calendar-day boundary, and its rolled files carry a full wall-clock
// timestamp (extractor-2006-01-02T15-04-05.000.log) rather than the
// date + iterator scheme asked for here. Matching both requirements on top
// of it is more code, and more surprising code, than this file.
//
// ▶ Why lazy rotation (checked on Write) rather than a midnight timer:
// the extractor logs on essentially every request, so the first write after
// midnight rolls the file with no perceptible delay, and a genuinely idle
// day simply produces no file for that day instead of an empty one. No
// background goroutine to own or shut down.
package logrotate

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const dateLayout = "2006-01-02"

// Writer is an io.Writer (and io.Closer) that rotates its backing file by day
// and by size. It is safe for concurrent use.
type Writer struct {
	dir      string // directory the files live in
	stem     string // base name with the final extension removed, e.g. "extractor"
	ext      string // final extension including the dot, e.g. ".log" ("" if none)
	maxBytes int64  // size cap; <= 0 disables size-based rotation (daily still applies)

	// now is the clock. Real code leaves it as time.Now; tests replace it to
	// drive day boundaries deterministically.
	now func() time.Time

	mu      sync.Mutex
	file    *os.File
	curDate string // dateLayout string of the currently open file
	curSeq  int    // iterator of the currently open file (0 == first of the day)
	curSize int64  // bytes in the currently open file, tracked to avoid a Stat per Write
}

// New returns a Writer that rotates the file at path. path is a full file
// path; its directory is created if missing, its base name is split into a
// stem and a final extension, and the date and iterator are spliced in
// between the two.
//
// maxBytes is the per-file size cap in bytes; pass <= 0 to rotate on the day
// boundary only.
//
// No file is opened until the first Write, so an idle day leaves no trace.
func New(path string, maxBytes int64) (*Writer, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("logrotate: create %s: %w", dir, err)
	}
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	return &Writer{
		dir:      dir,
		stem:     strings.TrimSuffix(base, ext),
		ext:      ext,
		maxBytes: maxBytes,
		now:      time.Now,
	}, nil
}

// Write implements io.Writer. The whole call is serialised so a concurrent
// rotation cannot close the file mid-write.
//
// A log record is never split across files: if appending p would push the
// current file past maxBytes, the file is rotated first and p is written
// whole into the fresh one (so the file may overshoot the cap by up to one
// record). A single record larger than maxBytes is written whole into an
// otherwise-empty file rather than triggering an unbounded rotate loop.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	date := w.now().Format(dateLayout)

	if w.file == nil || date != w.curDate {
		seq, size, err := w.highestSeqFor(date)
		if err != nil {
			return 0, err
		}
		if err := w.openLocked(date, seq, size); err != nil {
			return 0, err
		}
	}

	if w.maxBytes > 0 && w.curSize > 0 && w.curSize+int64(len(p)) > w.maxBytes {
		if err := w.openLocked(date, w.curSeq+1, 0); err != nil {
			return 0, err
		}
	}

	n, err := w.file.Write(p)
	w.curSize += int64(n)
	return n, err
}

// Close closes the current file, if one is open. The Writer must not be used
// afterwards. Provided mainly for tests; the long-running server never shuts
// its logger down.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// openLocked switches the Writer to dir/nameFor(date, seq), opening it for
// append (creating it if absent), and records startSize as the count of
// bytes already in it. Caller holds w.mu.
func (w *Writer) openLocked(date string, seq int, startSize int64) error {
	name := filepath.Join(w.dir, w.nameFor(date, seq))
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("logrotate: open %s: %w", name, err)
	}
	if w.file != nil {
		_ = w.file.Close()
	}
	w.file = f
	w.curDate = date
	w.curSeq = seq
	w.curSize = startSize
	return nil
}

// nameFor builds the file name for a given date and iterator. The iterator is
// omitted entirely for seq 0 so the common single-file day reads cleanly.
func (w *Writer) nameFor(date string, seq int) string {
	if seq <= 0 {
		return w.stem + "-" + date + w.ext
	}
	return w.stem + "-" + date + "." + strconv.Itoa(seq) + w.ext
}

// highestSeqFor scans w.dir for files already belonging to date and returns
// the largest iterator seen together with that file's current size, so a
// process restart part-way through a day appends to the file it left off on
// rather than clobbering it. When no file exists yet it returns (0, 0, nil):
// openLocked will then create the seq-0 file, and the size guard in Write
// rolls it forward on the first record if it is already at the cap.
func (w *Writer) highestSeqFor(date string) (seq int, size int64, err error) {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return 0, 0, fmt.Errorf("logrotate: read dir %s: %w", w.dir, err)
	}

	prefix := w.stem + "-" + date // exact, so "2026-08-3" cannot match "2026-08-30"
	best := -1
	var bestName string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, w.ext) {
			continue
		}
		mid := name[len(prefix) : len(name)-len(w.ext)]
		n, ok := parseIterator(mid)
		if !ok {
			continue
		}
		if n > best {
			best, bestName = n, name
		}
	}

	if best < 0 {
		return 0, 0, nil
	}
	info, statErr := os.Stat(filepath.Join(w.dir, bestName))
	if statErr != nil {
		// The dir listing saw it a moment ago; treat a vanished file as
		// "start fresh at this seq" rather than failing the write.
		return best, 0, nil
	}
	return best, info.Size(), nil
}

// parseIterator interprets the slice of a file name between the "stem-date"
// prefix and the extension: "" is iterator 0, ".<digits>" is that number,
// anything else is not one of our files.
func parseIterator(mid string) (int, bool) {
	if mid == "" {
		return 0, true
	}
	if !strings.HasPrefix(mid, ".") {
		return 0, false
	}
	n, err := strconv.Atoi(mid[1:])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
