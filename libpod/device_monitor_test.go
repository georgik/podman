//go:build !remote

package libpod

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.podman.io/podman/v6/libpod/define"
	"go.podman.io/podman/v6/libpod/lock"
)

// stubRebindMonitorExec replaces rebindMonitorExec for the duration of a test
// and returns a recorder of the "dst|hostDev" strings it was called with.
func stubRebindMonitorExec(t *testing.T) (*[]string, func()) {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	orig := rebindMonitorExec
	rebindMonitorExec = func(_ *Container, dst, hostDev string) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, dst+"|"+hostDev)
		return nil
	}
	return &calls, func() { rebindMonitorExec = orig }
}

// newRebindTestCtr returns a Running test container (valid: true) so the
// monitor's lightweight Running check passes. Pass running=false to mark it
// Stopped.
func newRebindTestCtr(t *testing.T, running bool) *Container {
	t.Helper()
	m, err := lock.NewInMemoryManager(16)
	require.NoError(t, err)
	c, err := getTestCtr1(m)
	require.NoError(t, err)
	if !running {
		c.lock.Lock()
		c.state.State = define.ContainerStateStopped
		c.lock.Unlock()
	}
	return c
}

// TestRebindMonitorReconcile_match covers the idempotent case: with the golden
// inode equal to the current host inode, the loop is a no-op on every tick
// (it must never re-rebind an up-to-date device).
func TestRebindMonitorReconcile_match(t *testing.T) {
	calls, restore := stubRebindMonitorExec(t)
	defer restore()

	src := filepath.Join(os.TempDir(), "rebind-monitor-match")
	require.NoError(t, os.WriteFile(src, []byte("x"), 0o644))
	defer os.Remove(src)
	gold, err := statSource(src)
	require.NoError(t, err)

	r := &Runtime{}
	c := newRebindTestCtr(t, true)
	r.registerRebindContainer(c, []rebindMount{{src: src, dst: "/dev/ttyACM0", want: gold}})

	r.reconcileAllRebindDevices()
	r.reconcileAllRebindDevices()
	require.Empty(t, *calls, "no rebind when the golden inode already matches the host inode")
}

// TestRebindMonitorReconcile_divergence covers the recovery case. The golden
// inode is deliberately stale (as if a re-enumeration already happened between
// registration and the first tick): the loop rebinds exactly once, records the
// fresh inode so the next tick is a no-op (i.e. it does not spin re-rebinding
// every tick — the failure mode of the old per-container watcher), and binds
// to the resolved host device name.
func TestRebindMonitorReconcile_divergence(t *testing.T) {
	calls, restore := stubRebindMonitorExec(t)
	defer restore()

	src := filepath.Join(os.TempDir(), "rebind-monitor-diverge")
	require.NoError(t, os.WriteFile(src, []byte("x"), 0o644))
	defer os.Remove(src)
	current, err := statSource(src)
	require.NoError(t, err)

	r := &Runtime{}
	c := newRebindTestCtr(t, true)
	// Deliberately stale golden inode: want != current host inode.
	stale := recordedInode{Major: 999, Minor: 999, Ino: 999999}
	r.registerRebindContainer(c, []rebindMount{{src: src, dst: "/dev/ttyACM0", want: stale}})

	// First tick: divergence -> exactly one rebind, to the resolved name.
	r.reconcileAllRebindDevices()
	require.Len(t, *calls, 1)
	assert.Equal(t, "/dev/ttyACM0|"+filepath.Base(src), (*calls)[0])

	// Second tick: golden inode now tracks the host inode -> no re-rebind.
	r.reconcileAllRebindDevices()
	require.Len(t, *calls, 1)

	// The stored golden inode was updated to the current host inode.
	r.rebindMu.Lock()
	stored := r.rebindContainers[c.ID()].mounts[0].want
	r.rebindMu.Unlock()
	assert.Equal(t, current, stored)
}

// TestRebindMonitorSkipsStopped verifies the "never exit on a transient
// condition, just skip" behavior: a stopped container is not rebound, and an
// absent source is retried rather than recorded as up to date.
func TestRebindMonitorSkipsStopped(t *testing.T) {
	t.Run("stopped container is skipped", func(t *testing.T) {
		calls, restore := stubRebindMonitorExec(t)
		defer restore()

		r := &Runtime{}
		c := newRebindTestCtr(t, false) // Stopped
		src := filepath.Join(os.TempDir(), "rebind-monitor-stopped")
		require.NoError(t, os.WriteFile(src, []byte("x"), 0o644))
		defer os.Remove(src)
		gold, err := statSource(src)
		require.NoError(t, err)
		r.registerRebindContainer(c, []rebindMount{{src: src, dst: "/dev/ttyACM0", want: gold}})

		r.reconcileAllRebindDevices()
		require.Empty(t, *calls, "stopped container must not be rebound")
	})

	t.Run("absent source is skipped, golden unchanged", func(t *testing.T) {
		calls, restore := stubRebindMonitorExec(t)
		defer restore()

		r := &Runtime{}
		c := newRebindTestCtr(t, true)
		absent := filepath.Join(os.TempDir(), "rebind-monitor-absent-nope")
		r.registerRebindContainer(c, []rebindMount{{src: absent, dst: "/dev/ttyACM0", want: recordedInode{166, 0, 4242}}})

		r.reconcileAllRebindDevices()
		require.Empty(t, *calls, "absent source must not trigger a rebind")
	})
}
