package controllers

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
)

// findReloadTargets scans procRoot (normally /proc) for the co-located
// proxy process — argv0 basename == name — excluding the scanner
// itself (`self`). With the pod's shareProcessNamespace:true the
// operator sidecar sees that PID here. Pure + procRoot/name-
// parameterized so it is unit-testable without real processes.
func findReloadTargets(procRoot string, self int, name string) []int {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "cmdline"))
		if err != nil || len(raw) == 0 {
			continue
		}
		argv0 := string(raw)
		if i := strings.IndexByte(argv0, 0); i >= 0 {
			argv0 = argv0[:i]
		}
		if argv0 == "" {
			continue
		}
		if filepath.Base(argv0) == name {
			pids = append(pids, pid)
		}
	}
	return pids
}

// reloadDebouncer collapses SIGHUP bursts. The leading edge fires
// immediately; any triggers within `window` of the last fire collapse
// into a single trailing fire scheduled at the end of the window — so
// rapid churn (e.g. cert-manager creating/deleting solver objects)
// does not produce a SIGHUP storm, while the final state is ALWAYS
// applied (trailing edge guarantees eventual consistency). window<=0
// disables debouncing (every trigger fires immediately).
type reloadDebouncer struct {
	mu      sync.Mutex
	window  time.Duration
	last    time.Time
	pending bool
	do      func()
}

func newReloadDebouncer(window time.Duration, do func()) *reloadDebouncer {
	return &reloadDebouncer{window: window, do: do}
}

func (d *reloadDebouncer) trigger() {
	d.mu.Lock()
	now := time.Now()
	if d.window <= 0 || now.Sub(d.last) >= d.window {
		d.last = now
		d.mu.Unlock()
		d.do()
		return
	}
	if d.pending {
		d.mu.Unlock()
		return
	}
	d.pending = true
	wait := d.window - now.Sub(d.last)
	d.mu.Unlock()
	time.AfterFunc(wait, func() {
		d.mu.Lock()
		d.pending = false
		d.last = time.Now()
		d.mu.Unlock()
		d.do()
	})
}

// ReloadSignaler SIGHUPs a co-located synapse process so it
// deterministically re-reads its config (synapse's SIGHUP handler
// broadcasts a reload; the upstreams filewatch's reload arm re-reads with
// no debounce and BYPASSES its content-digest gate). Independent of
// inotify event types and timing. Bursts are coalesced via
// reloadDebouncer.
//
// Standalone rather than a method set on IngressReconciler so the
// config-sync sidecar can reuse it — both modes run beside synapse in the
// same pod and need exactly this behaviour.
//
// TWO things must hold for the signal to land, and both have been observed
// failing in a live cluster:
//
//  1. uid parity. The operator image is distroless nonroot (USER 65532)
//     while synapse runs as root, so kill returns EPERM unless the sidecar
//     is given runAsUser: 0 (or CAP_KILL).
//  2. AppArmor must permit it. On an AppArmor-enforcing host the sidecar
//     runs under containerd's default profile, and that profile only allows
//     signalling peers in the SAME profile. A privileged synapse is
//     unconfined, so the kernel denies SIGHUP regardless of uid or CAP_KILL:
//     apparmor="DENIED" operation="signal" signal=hup peer="unconfined".
//     The sidecar therefore also needs appArmorProfile: Unconfined.
//
// Failure is degraded, not fatal: synapse still picks the file up via its
// own inotify watch, just after the 500ms settle debounce instead of
// immediately. doReload logs rather than failing silently.
type ReloadSignaler struct {
	// ProcessName is the argv0 basename to look for; "" means "synapse".
	ProcessName string
	// Debounce coalesces SIGHUP bursts; <=0 signals on every trigger.
	Debounce time.Duration

	once sync.Once
	d    *reloadDebouncer
}

// Signal requests a reload, coalescing bursts within the debounce window.
func (s *ReloadSignaler) Signal() {
	s.once.Do(func() {
		s.d = newReloadDebouncer(s.Debounce, func() {
			s.doReload(ctrl.Log.WithName("reload"))
		})
	})
	s.d.trigger()
}

func (s *ReloadSignaler) doReload(logger logr.Logger) {
	name := s.ProcessName
	if name == "" {
		name = "synapse"
	}
	pids := findReloadTargets("/proc", os.Getpid(), name)
	if len(pids) == 0 {
		logger.Info("config changed but no target process found to SIGHUP "+
			"(shareProcessNamespace not enabled, or process not started yet)", "process", name)
		return
	}
	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGHUP); err != nil {
			// EPERM here almost always means the sidecar and synapse run as
			// different uids — see the type comment.
			logger.Error(err, "SIGHUP failed - check uid parity (runAsUser 0) AND "+
				"AppArmor (sidecar needs appArmorProfile Unconfined); "+
				"falling back to synapse inotify watch",
				"pid", pid, "process", name)
		} else {
			logger.Info("SIGHUP → reload", "pid", pid, "process", name)
			mReloadTotal.Inc()
		}
	}
}

// signalReload keeps the IngressReconciler call site unchanged; it just
// delegates to a lazily-built reloadSignaler.
func (r *IngressReconciler) signalReload(ctx context.Context) {
	r.reloadOnce.Do(func() {
		r.signaler = &ReloadSignaler{
			ProcessName: r.ReloadProcessName,
			Debounce:    r.ReloadDebounce,
		}
	})
	_ = ctx
	r.signaler.Signal()
}
