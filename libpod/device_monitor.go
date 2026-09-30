//go:build !remote

package libpod

import (
	"path/filepath"
	"time"

	"github.com/sirupsen/logrus"
	"go.podman.io/podman/v6/libpod/define"
)

// rebindMonitorInterval is how often the global rebind monitor probes the host
// device inodes. A USB power cycle is a rare event, so a 500ms poll is plenty
// responsive while keeping the daemon idle most of the time.
const rebindMonitorInterval = 500 * time.Millisecond

// rebindMonitorExec performs the actual rebind for a single mount. It is a
// package variable (like umountFn/mountBindFn in device_reconcile.go) so tests
// can stub it; in production it drives umount/mount --bind inside the
// container's mount namespace via (*Container).rebindViaExec.
var rebindMonitorExec = func(c *Container, dst string, hostDev string) error {
	return c.rebindViaExec(dst, hostDev)
}

// rebindContainerState is the monitor's per-container record: the container and
// the rebind mounts parsed once at registration. Storing the parsed mounts (and
// their golden inodes) in memory lets the monitor update the "last known good"
// inode after a rebind, so it does not re-rebind the same device on every tick.
type rebindContainerState struct {
	c      *Container
	mounts []rebindMount
}

// StartRebindMonitor launches the daemon's global rebind monitor. It is
// idempotent (guarded by sync.Once) and is meant to be called once from the
// daemon entrypoint. The monitor lives for the lifetime of the Runtime: it
// exits when Shutdown closes rebindMonitorDone or when r.valid becomes false.
// It performs an initial scan so containers already running when the daemon
// started are covered without waiting for a restart.
func (r *Runtime) StartRebindMonitor() {
	r.rebindMonitorOnce.Do(func() {
		if r.rebindMonitorDone == nil {
			r.rebindMonitorDone = make(chan struct{})
		}
		r.rebindMonitorGroup.Add(1)
		go r.runRebindMonitor()
	})
}

// runRebindMonitor is the monitor loop. It never exits on a transient
// per-container condition: the only exits are the daemon shutting down
// (rebindMonitorDone closed, or r.valid cleared by Shutdown).
func (r *Runtime) runRebindMonitor() {
	defer r.rebindMonitorGroup.Done()
	ticker := time.NewTicker(rebindMonitorInterval)
	defer ticker.Stop()

	// Initial scan: cover containers that were already running when the daemon
	// started (e.g. after a `podman system service` restart).
	r.rebindStartupScan()

	for {
		select {
		case <-r.rebindMonitorDone:
			return
		case <-ticker.C:
			if !r.valid {
				return
			}
			r.reconcileAllRebindDevices()
		}
	}
}

// reconcileAllRebindDevices reconciles every registered rebind container. It
// never blocks on a single container for long and never exits on a transient
// failure: a container that is not running, whose spec cannot be read, or
// whose rebind fails, is simply skipped and retried on the next tick.
func (r *Runtime) reconcileAllRebindDevices() {
	r.rebindMu.Lock()
	targets := make([]*rebindContainerState, 0, len(r.rebindContainers))
	for _, s := range r.rebindContainers {
		targets = append(targets, s)
	}
	r.rebindMu.Unlock()

	for _, s := range targets {
		c := s.c
		// Lightweight Running check: read the state under the container lock,
		// then release before any I/O/exec so we never hold the lock across an
		// exec session (which would deadlock). We deliberately do NOT call
		// syncContainer() here: it is heavy and racy, and a slightly stale
		// state is harmless (a failed rebind is simply retried next tick).
		c.lock.Lock()
		running := c.valid && c.state.State == define.ContainerStateRunning
		c.lock.Unlock()
		if !running {
			continue
		}

		for i := range s.mounts {
			m := s.mounts[i]
			got, err := statSource(rebindStatPath(m.src, m.byId))
			if err != nil {
				// Source absent right now (powered off / not yet
				// re-enumerated). Leave it for the next tick and do NOT
				// update the golden inode.
				continue
			}
			if got == m.want {
				// The mount already points at the current inode.
				continue
			}
			// Diverged: resolve the CURRENT host device name (robust to the tty
			// number changing after re-enumeration) and rebind inside the
			// container.
			devName, err := resolveRebindDevName(m)
			if err != nil {
				logrus.Debugf("reconcile: resolving current device for %s: %v", m.dst, err)
				continue
			}
			if rerr := rebindMonitorExec(c, m.dst, devName); rerr != nil {
				// Transient failure (e.g. mount --bind blocked in a rootless
				// user namespace, or the container stopped mid-rebind). Log
				// quietly and retry next tick.
				logrus.Debugf("reconcile: rebind %s to %s failed, will retry: %v", m.dst, devName, rerr)
				continue
			}
			// Record the inode we just rebound to so the next pass sees the
			// device as up to date until the next power cycle.
			s.mounts[i] = rebindMount{src: m.src, dst: m.dst, byId: m.byId, want: got}
			logrus.Infof("reconcile: rebound device %s on container %s to its fresh host inode", m.dst, c.ID())
		}
	}
}

// rebindStartupScan registers every already-running container that exposes
// rebind devices, so a daemon started while containers are running still covers
// them without waiting for a restart.
func (r *Runtime) rebindStartupScan() {
	ctrs, err := r.GetRunningContainers()
	if err != nil {
		logrus.Debugf("reconcile: initial rebind scan: listing containers: %v", err)
		return
	}
	for _, c := range ctrs {
		mounts, err := c.rebindDeviceMounts()
		if err != nil || len(mounts) == 0 {
			continue
		}
		r.registerRebindContainer(c, mounts)
	}
}

// registerRebindContainer records a container's rebind mounts in the monitor.
// Idempotent: re-registering replaces any prior record (e.g. on restart).
func (r *Runtime) registerRebindContainer(c *Container, mounts []rebindMount) {
	r.rebindMu.Lock()
	defer r.rebindMu.Unlock()
	if r.rebindContainers == nil {
		r.rebindContainers = make(map[string]*rebindContainerState)
	}
	r.rebindContainers[c.ID()] = &rebindContainerState{c: c, mounts: mounts}
	logrus.Infof("reconcile: monitoring rebind devices on container %s (%d device(s))", c.ID(), len(mounts))
}

// unregisterRebindContainer forgets a container's rebind record. It is safe to
// call from teardown paths that already hold the container lock: it takes only
// rebindMu, so it never deadlocks against the monitor's own exec session.
func (r *Runtime) unregisterRebindContainer(id string) {
	r.rebindMu.Lock()
	defer r.rebindMu.Unlock()
	if _, ok := r.rebindContainers[id]; ok {
		delete(r.rebindContainers, id)
		logrus.Debugf("reconcile: stopped monitoring rebind devices for container %s", id)
	}
}

// resolveRebindDevName returns the host device's CURRENT name to bind: the
// /dev/serial/by-id link when recorded (so a renumbered tty is followed),
// else the basename of the recorded source path. It is resolved in the host
// (daemon) namespace; the returned name is then bound inside the container from
// rebindHostDevDir.
func resolveRebindDevName(m rebindMount) (string, error) {
	if m.byId != "" {
		target, err := filepath.EvalSymlinks(filepath.Join(rebindSerialByIdDir, m.byId))
		if err != nil {
			return "", err
		}
		return filepath.Base(target), nil
	}
	return filepath.Base(m.src), nil
}
