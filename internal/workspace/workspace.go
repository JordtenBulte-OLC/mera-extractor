// internal/workspace/workspace.go
//
// Package workspace owns the directory under MERA_WORK_ROOT where git clones
// are materialised, and the two mechanisms that keep that root from filling
// up with the debris of crashed or killed requests.
//
// ▶ Why a package of its own, and not just a couple of helpers in gitops:
// gitops.CloneBoth / gitops.Clone are stateless — they take a root, make a
// "clone-*" dir under it, and hand it back. Keeping the root from growing
// without bound is a separate, stateful concern (a background goroutine, an
// in-memory set of live dirs, an instance identity) that has no business
// living inside the clone functions. gitops is left untouched: it still
// receives a root and makes "clone-*" dirs under it — the root it receives
// is now Manager.Dir() instead of MERA_WORK_ROOT directly.
//
// The three layers, and the failure each one covers (see the table in
// MERA-session-status.md's "workspace cleanup" note):
//
//  1. Per-request defer d.Cleanup(workDir)  — the api layer already does this;
//     covers a normal return and a panic that unwinds through the handler.
//  2. Startup sweep of THIS instance's own dir — covers a SIGKILL / OOM-kill /
//     container restart that killed the process before its defers could run.
//     Safe because at startup this process has begun no requests and the
//     per-instance dir is keyed to (hostname, pid) so nothing else owns it.
//  3. Background janitor — covers a *different*, now-dead instance's dir
//     (rolling deploy, container recreate, a crashed `go run`). It only ever
//     descends into directories carrying this package's .owner marker, and
//     only removes a "clone-*" dir that has been idle past a TTL and is not
//     in the live set — so it is safe even when MERA_WORK_ROOT is a shared
//     /tmp.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// ownerFile marks a directory as one this package created. The janitor
	// descends into a subdirectory of the root ONLY if it contains this file,
	// which is what lets it run safely against a root it does not exclusively
	// own (the local-dev default root is os.TempDir()).
	ownerFile = ".owner"

	// heartbeatFile is refreshed by Track while a request holds a workspace.
	// Its mtime is the "still in use" signal the janitor reads for a dir it
	// cannot find in its in-memory live set (e.g. a future cross-process
	// janitor, or this one racing a Track that has not registered yet).
	heartbeatFile = ".heartbeat"

	// workspaceNamePrefix is what gitops names its dirs: CloneBoth uses
	// os.MkdirTemp(root, "clone-both-*"), Clone uses "clone-*". Both share
	// this prefix and nothing else under an instance dir does.
	workspaceNamePrefix = "clone-"
)

// Defaults. All three are overridable through the With* options; only the TTL
// is wired to an env var (MERA_WORKSPACE_TTL) in main.
const (
	DefaultTTL       = 30 * time.Minute
	DefaultSweep     = 5 * time.Minute
	DefaultHeartbeat = 60 * time.Second
)

// Manager is created once per process. The zero value is not usable — call
// New. A nil *Manager is, however, a valid receiver for Track and
// StartJanitor: that is what lets the api layer treat a wired Manager as
// optional (tests construct a Server with none).
type Manager struct {
	root  string        // the shared root, e.g. /work
	dir   string        // root/<id> — where workspaces are actually made
	id    string        // this process's instance identity
	ttl   time.Duration // a clone-* dir idle this long is an orphan
	sweep time.Duration // janitor tick interval; <=0 disables the janitor
	hb    time.Duration // heartbeat write interval for a tracked workspace
	logf  func(string, ...any)

	mu     sync.Mutex
	active map[string]bool // absolute workspace paths a request currently holds
}

// Option configures a Manager at construction.
type Option func(*Manager)

// WithTTL sets how long a clone dir may sit idle before the janitor reclaims
// it. Values <= 0 are ignored (the default stands).
func WithTTL(d time.Duration) Option {
	return func(m *Manager) {
		if d > 0 {
			m.ttl = d
		}
	}
}

// WithSweepInterval sets the janitor tick. A value <= 0 disables the janitor
// entirely (StartJanitor becomes a no-op) — useful in tests that drive reap
// directly.
func WithSweepInterval(d time.Duration) Option {
	return func(m *Manager) { m.sweep = d }
}

// WithHeartbeatInterval sets how often Track rewrites the heartbeat file.
// Values <= 0 are ignored.
func WithHeartbeatInterval(d time.Duration) Option {
	return func(m *Manager) {
		if d > 0 {
			m.hb = d
		}
	}
}

// WithLogf redirects the Manager's log output. The default is log.Printf.
// The janitor and the heartbeat run in the background with no caller to
// return an error to, so they must log; this is the seam a test uses to
// capture or silence that.
func WithLogf(f func(string, ...any)) Option {
	return func(m *Manager) {
		if f != nil {
			m.logf = f
		}
	}
}

// New picks this process's instance identity, creates root/<id> (and root
// itself if needed), drops the .owner marker, and runs the startup sweep of
// that dir. It returns an error only when the instance dir cannot be created
// — without a writable workspace dir the process cannot do anything useful,
// so main treats that as fatal.
func New(root string, opts ...Option) (*Manager, error) {
	if root == "" {
		return nil, errors.New("workspace: root is required")
	}
	m := &Manager{
		root:   root,
		id:     instanceID(),
		ttl:    DefaultTTL,
		sweep:  DefaultSweep,
		hb:     DefaultHeartbeat,
		logf:   log.Printf,
		active: map[string]bool{},
	}
	for _, o := range opts {
		o(m)
	}
	m.dir = filepath.Join(root, m.id)

	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return nil, fmt.Errorf("workspace: create instance dir %s: %w", m.dir, err)
	}
	m.writeOwner()
	m.sweepOwn()
	return m, nil
}

// Dir is the directory to hand to gitops.CloneBoth / gitops.Clone as their
// work root. Everything they create lands under it.
func (m *Manager) Dir() string { return m.dir }

// Describe is a one-line summary for the startup log.
func (m *Manager) Describe() string {
	return fmt.Sprintf("instance=%s dir=%s ttl=%s janitor=%s heartbeat=%s",
		m.id, m.dir, m.ttl, m.sweep, m.hb)
}

// Track marks workDir as in use and starts a goroutine that refreshes its
// heartbeat file every hb interval. The returned stop function halts that
// goroutine and unmarks the dir; it is safe to call exactly once via defer,
// and safe to call on the result of a nil-Manager Track.
//
// ▶ Why both an in-memory set AND a heartbeat file: the set is the
// authoritative "this process is using it right now" and needs no clock; the
// file is the fallback for the window between CloneBoth returning and Track
// registering, and the only signal a cross-process janitor could ever read.
// A dir is spared if EITHER says it is live.
func (m *Manager) Track(workDir string) (stop func()) {
	if m == nil || workDir == "" {
		return func() {}
	}

	m.mu.Lock()
	m.active[workDir] = true
	m.mu.Unlock()

	done := make(chan struct{})
	go m.beat(workDir, done)

	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			m.mu.Lock()
			delete(m.active, workDir)
			m.mu.Unlock()
		})
	}
}

func (m *Manager) beat(workDir string, done <-chan struct{}) {
	write := func() {
		p := filepath.Join(workDir, heartbeatFile)
		stamp := time.Now().UTC().Format(time.RFC3339) + "\n"
		if err := os.WriteFile(p, []byte(stamp), 0o644); err != nil {
			// Degrade, don't fail: a missed heartbeat only matters if the
			// request also outlives the TTL, and even then the live set
			// still protects it while this process is up.
			m.logf("workspace: heartbeat write failed for %s: %v", workDir, err)
		}
	}
	write() // don't make the first request wait a whole interval to be marked fresh

	t := time.NewTicker(m.hb)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			write()
		}
	}
}

// StartJanitor launches the background reaper. It stops when ctx is done. A
// nil Manager or a non-positive sweep interval makes this a no-op.
func (m *Manager) StartJanitor(ctx context.Context) {
	if m == nil || m.sweep <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(m.sweep)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.reap()
			}
		}
	}()
}

// reap scans the root for instance dirs (identified by their .owner marker)
// and removes any clone-* workspace under them that has been idle past the
// TTL and is not currently tracked. This includes THIS instance's own dir —
// a workspace left by a crashed request in a still-running process is just as
// much an orphan as one from a dead process.
func (m *Manager) reap() {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		m.logf("workspace: janitor could not read %s: %v", m.root, err)
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		instDir := filepath.Join(m.root, e.Name())

		// The one safety gate: only ever look inside a directory this package
		// stamped. A shared /tmp is full of other people's dirs and some of
		// them will be named "clone-*"; none of them have our .owner file.
		if !fileExists(filepath.Join(instDir, ownerFile)) {
			continue
		}

		kids, err := os.ReadDir(instDir)
		if err != nil {
			m.logf("workspace: janitor could not read %s: %v", instDir, err)
			continue
		}
		for _, k := range kids {
			if k.IsDir() && isWorkspaceName(k.Name()) {
				m.consider(filepath.Join(instDir, k.Name()))
			}
		}
	}
}

func (m *Manager) consider(path string) {
	m.mu.Lock()
	live := m.active[path]
	m.mu.Unlock()
	if live {
		return
	}

	idle := time.Since(m.newest(path))
	if idle <= m.ttl {
		return
	}
	if err := os.RemoveAll(path); err != nil {
		m.logf("workspace: janitor could not remove %s (idle %s): %v", path, idle.Round(time.Second), err)
		return
	}
	m.logf("workspace: janitor reclaimed %s (idle %s)", path, idle.Round(time.Second))
}

// sweepOwn removes every clone-* dir directly under this instance's dir. Run
// once, from New, before any request can start. Unconditional on purpose:
// the dir is keyed to (hostname, pid); another live process on the same host
// has a different pid, and the same pid on the same host means the previous
// owner is gone and this is its restart.
func (m *Manager) sweepOwn() {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		m.logf("workspace: startup sweep could not read %s: %v", m.dir, err)
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !isWorkspaceName(e.Name()) {
			continue
		}
		p := filepath.Join(m.dir, e.Name())
		if err := os.RemoveAll(p); err != nil {
			m.logf("workspace: startup sweep could not remove %s: %v", p, err)
			continue
		}
		m.logf("workspace: startup sweep reclaimed leftover %s", p)
	}
}

func (m *Manager) writeOwner() {
	body := fmt.Sprintf("instance %s\nstarted  %s\npid      %d\n",
		m.id, time.Now().UTC().Format(time.RFC3339), os.Getpid())
	if err := os.WriteFile(filepath.Join(m.dir, ownerFile), []byte(body), 0o644); err != nil {
		// Non-fatal: .owner is a human convenience AND the janitor's descend
		// gate. If it cannot be written, this instance's dir simply will not
		// be reaped by a peer — the startup sweep still covers its restarts.
		m.logf("workspace: could not write %s in %s: %v", ownerFile, m.dir, err)
	}
}

// newest returns the later of the workspace dir's own mtime and its heartbeat
// file's mtime. A missing file reports the zero time, which makes it "very
// old" — correct for a workspace whose request died without ever heart-
// beating.
func (m *Manager) newest(dir string) time.Time {
	t := statModTime(dir)
	if hb := statModTime(filepath.Join(dir, heartbeatFile)); hb.After(t) {
		t = hb
	}
	return t
}

func isWorkspaceName(name string) bool {
	return strings.HasPrefix(name, workspaceNamePrefix)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func statModTime(p string) time.Time {
	fi, err := os.Stat(p)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// instanceID is (hostname, pid). See the package doc for why this shape gives
// "restart of the same container reclaims its own leftovers" while keeping
// concurrent local `go run` processes from colliding.
func instanceID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	// A hostname (or container ID) is normally already path-safe; map the few
	// characters that would let it escape or split the path, just in case.
	host = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', ' ':
			return '-'
		default:
			return r
		}
	}, host)
	return host + "-" + strconv.Itoa(os.Getpid())
}
