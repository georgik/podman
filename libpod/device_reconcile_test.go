//go:build !remote

package libpod

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestParseRebindOpt(t *testing.T) {
	t.Run("records inodes and pending", func(t *testing.T) {
		inodes, byId, pending := parseRebindOpt([]string{
			"slave", "nosuid", "noexec", "rw", "rbind",
			"x-podman-dev-ino=189:5:12345",
			"x-podman-dev-pending",
		})
		require.Len(t, inodes, 1)
		assert.Equal(t, recordedInode{Major: 189, Minor: 5, Ino: 12345}, inodes[0])
		assert.Empty(t, byId)
		assert.True(t, pending)
	})

	t.Run("records by-id link name", func(t *testing.T) {
		inodes, byId, pending := parseRebindOpt([]string{
			"x-podman-dev-ino=166:0:1366",
			"x-podman-dev-by-id=usb-generic-serial-00:11:22:33:44:55-if00",
		})
		require.Len(t, inodes, 1)
		// The by-id string keeps its colons (a MAC) verbatim.
		assert.Equal(t, "usb-generic-serial-00:11:22:33:44:55-if00", byId)
		assert.False(t, pending)
	})

	t.Run("multiple inodes", func(t *testing.T) {
		inodes, byId, pending := parseRebindOpt([]string{
			"x-podman-dev-ino=189:5:1",
			"x-podman-dev-ino=188:0:2",
		})
		require.Len(t, inodes, 2)
		assert.Equal(t, recordedInode{189, 5, 1}, inodes[0])
		assert.Equal(t, recordedInode{188, 0, 2}, inodes[1])
		assert.Empty(t, byId)
		assert.False(t, pending)
	})

	t.Run("ignores malformed entries", func(t *testing.T) {
		// No inodes, no by-id, no pending: malformed/prefix-only entries are skipped.
		inodes, byId, pending := parseRebindOpt([]string{
			"x-podman-dev-ino=not:a:number",
			"x-podman-dev-ino=1",
			"x-podman-dev-by-id=",
			"garbage",
		})
		assert.Empty(t, inodes)
		assert.Empty(t, byId)
		assert.False(t, pending)
	})

	t.Run("pending only", func(t *testing.T) {
		inodes, byId, pending := parseRebindOpt([]string{"x-podman-dev-pending"})
		assert.Empty(t, inodes)
		assert.Empty(t, byId)
		assert.True(t, pending)
	})
}

func TestReconcileDeviceMount(t *testing.T) {
	// Stub the rebind operations so we can assert they are (or are not) called
	// without touching the real filesystem mounts. The rebind rebinds the
	// container's device node to the host's current inode, so the stub counts
	// mount --bind calls.
	tmp := os.TempDir()
	src := filepath.Join(tmp, "rebind-src-probe")
	require.NoError(t, os.WriteFile(src, []byte("x"), 0o644))
	defer os.Remove(src)

	// Capture the real inode of the source to build the "unchanged" case.
	got, err := statSource(src)
	require.NoError(t, err)

	t.Run("unchanged inode is a no-op", func(t *testing.T) {
		var umounts, knods int
		restore := stubRebindOps(t, func(dst string) error {
			umounts++
			return nil
		}, func(dst, hostDev string) error {
			knods++
			return nil
		})
		defer restore()

		rebounded, rerr := reconcileDeviceMount(src, "/dev/ttyACM0", "", got)
		require.NoError(t, rerr)
		assert.False(t, rebounded)
		assert.Zero(t, umounts)
		assert.Zero(t, knods)
	})

	t.Run("changed inode rebinds", func(t *testing.T) {
		var umounts, knods int
		restore := stubRebindOps(t, func(dst string) error {
			umounts++
			return nil
		}, func(dst, hostDev string) error {
			knods++
			return nil
		})
		defer restore()

		// Deliberately wrong inode so reconcile must rebind.
		rebounded, rerr := reconcileDeviceMount(src, "/dev/ttyACM0", "", recordedInode{got.Major, got.Minor, got.Ino + 1})
		require.NoError(t, rerr)
		assert.True(t, rebounded)
		assert.Equal(t, 1, umounts)
		assert.Equal(t, 1, knods)
	})

	t.Run("missing source is a no-op", func(t *testing.T) {
		var umounts, knods int
		restore := stubRebindOps(t, func(dst string) error {
			umounts++
			return nil
		}, func(dst, hostDev string) error {
			knods++
			return nil
		})
		defer restore()

		rebounded, rerr := reconcileDeviceMount("/dev/does-not-exist-probe", "/dev/ttyACM0", "", got)
		require.NoError(t, rerr)
		assert.False(t, rebounded)
		assert.Zero(t, umounts)
		assert.Zero(t, knods)
	})

	t.Run("umount failure aborts", func(t *testing.T) {
		var knods int
		restore := stubRebindOps(t, func(dst string) error {
			return errors.New("device or resource busy")
		}, func(dst, hostDev string) error {
			knods++
			return nil
		})
		defer restore()

		_, rerr := reconcileDeviceMount(src, "/dev/ttyACM0", "", recordedInode{got.Major, got.Minor, got.Ino + 1})
		require.Error(t, rerr)
		assert.Zero(t, knods)
	})

	t.Run("not-mounted umount is tolerated", func(t *testing.T) {
		var knods int
		restore := stubRebindOps(t, func(dst string) error {
			return errors.New("mountpoint: not mounted")
		}, func(dst, hostDev string) error {
			knods++
			return nil
		})
		defer restore()

		rebounded, rerr := reconcileDeviceMount(src, "/dev/ttyACM0", "", recordedInode{got.Major, got.Minor, got.Ino + 1})
		require.NoError(t, rerr)
		assert.True(t, rebounded)
		assert.Equal(t, 1, knods)
	})

	t.Run("stats the by-id link, not the source", func(t *testing.T) {
		// /dev/ttyACM0 has a /dev/serial/by-id symlink in this environment.
		// Pass a bogus source plus the by-id name: reconcile must stat the
		// by-id link (which resolves to the live board), so with a wrong want
		// it rebinds even though the source path does not exist.
		byId := serialByIdProbeName(t)
		if byId == "" {
			t.Skip("no /dev/serial/by-id char-device link available in this environment")
		}
		var umounts, knods int
		restore := stubRebindOps(t, func(dst string) error {
			umounts++
			return nil
		}, func(dst, hostDev string) error {
			knods++
			return nil
		})
		defer restore()

		rebounded, rerr := reconcileDeviceMount("/dev/bogus-source-probe", "/dev/ttyACM0", byId, recordedInode{1, 2, 3})
		require.NoError(t, rerr)
		assert.True(t, rebounded, "reconcile should rebind via the by-id link")
		assert.Equal(t, 1, umounts)
		assert.Equal(t, 1, knods)
	})
}

// serialByIdProbeName returns the name of the first /dev/serial/by-id symlink
// that resolves to a character device, or "" if none exists (so a test can be
// skipped on hosts without USB serial by-id links).
func serialByIdProbeName(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir("/dev/serial/by-id")
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		link := filepath.Join("/dev/serial/by-id", e.Name())
		var st unix.Statx_t
		if err := unix.Statx(int(unix.AT_FDCWD), link, 0, unix.STATX_ALL, &st); err == nil && st.Mode&unix.S_IFCHR != 0 {
			return e.Name()
		}
	}
	return ""
}

// returns a restore func.
func stubRebindOps(t *testing.T, umount func(string) error, mountBind func(string, string) error) func() {
	t.Helper()
	origU, origK := umountFn, mountBindFn
	umountFn = umount
	mountBindFn = mountBind
	return func() {
		umountFn, mountBindFn = origU, origK
	}
}
