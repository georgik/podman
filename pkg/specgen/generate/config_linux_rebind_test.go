//go:build !remote

package generate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	spec "github.com/opencontainers/runtime-spec/specs-go"
	rtgen "github.com/opencontainers/runtime-tools/generate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.podman.io/common/pkg/config"
	"go.podman.io/podman/v6/pkg/rootless"
	"golang.org/x/sys/unix"
)

func TestSplitDeviceSpec(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		wantPath   string
		wantRebind bool
	}{
		{"plain path", "/dev/ttyACM0", "/dev/ttyACM0", false},
		{"path with dst", "/dev/ttyACM0:/dev/ttyACM0", "/dev/ttyACM0:/dev/ttyACM0", false},
		{"path with mode", "/dev/ttyACM0:/dev/ttyACM0:rwm", "/dev/ttyACM0:/dev/ttyACM0:rwm", false},
		{"rebind trailing", "/dev/ttyACM0,rebind", "/dev/ttyACM0", true},
		{"persistent trailing", "/dev/ttyACM0,persistent", "/dev/ttyACM0", true},
		{"rebind case-insensitive", "/dev/ttyACM0,ReBind", "/dev/ttyACM0", true},
		{"rebind with dst", "/dev/ttyACM0:/dev/ttyACM0,rebind", "/dev/ttyACM0:/dev/ttyACM0", true},
		{"rebind with mode", "/dev/ttyACM0:/dev/ttyACM0:rwm,rebind", "/dev/ttyACM0:/dev/ttyACM0:rwm", true},
		{"non-rebind trailing token left alone", "/dev/ttyACM0,foo", "/dev/ttyACM0,foo", false},
		{"empty", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path, rebind := splitDeviceSpec(tc.in)
			assert.Equal(t, tc.wantPath, path)
			assert.Equal(t, tc.wantRebind, rebind)
		})
	}
}

func TestSplitDeviceSpec_doesNotClobberMode(t *testing.T) {
	// A device-mode token such as "rwm" must never be mistaken for the
	// rebind flag, even though it is a trailing colon-field.
	path, rebind := splitDeviceSpec("/dev/ttyACM0:rwm")
	require.Equal(t, "/dev/ttyACM0:rwm", path)
	assert.False(t, rebind)
}

// statxInode returns the (major,minor):ino of src, mirroring applyRebindOptions.
// It reads st_rdev (the device's own major:minor), exactly as applyRebindOptions
// records it, NOT st_dev (the filesystem/devtmpfs number, identical for every
// node on the same mount).
func statxInode(t *testing.T, src string) string {
	var st unix.Statx_t
	require.NoError(t, unix.Statx(int(unix.AT_FDCWD), src, 0, unix.STATX_ALL, &st))
	return fmt.Sprintf("%d:%d:%d", st.Rdev_major, st.Rdev_minor, st.Ino)
}

// serialByIdFor returns the /dev/serial/by-id/<id> link name that resolves to
// the given device source, or "" if none exists. It matches on the device's
// own device number (st_rdev), mirroring the production serialById helper.
func serialByIdFor(t *testing.T, src string) string {
	t.Helper()
	entries, err := os.ReadDir("/dev/serial/by-id")
	if err != nil {
		return ""
	}
	var st unix.Statx_t
	if err := unix.Statx(int(unix.AT_FDCWD), src, 0, unix.STATX_ALL, &st); err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		link := filepath.Join("/dev/serial/by-id", e.Name())
		var linkSt unix.Statx_t
		if err := unix.Statx(int(unix.AT_FDCWD), link, 0, unix.STATX_ALL, &linkSt); err != nil {
			continue
		}
		if linkSt.Rdev_major == st.Rdev_major && linkSt.Rdev_minor == st.Rdev_minor {
			return e.Name()
		}
	}
	return ""
}

// TestApplyRebindOptions records the source inode into the mount options and
// marks the device pending when the source is absent.
func TestApplyRebindOptions(t *testing.T) {
	src := filepath.Join(os.TempDir(), "rebind-opts-probe")
	require.NoError(t, os.WriteFile(src, []byte("x"), 0o644))
	defer os.Remove(src)

	t.Run("records inode when present", func(t *testing.T) {
		devMnt := &spec.Mount{}
		applyRebindOptions(src, devMnt)

		var inoOpt string
		for _, o := range devMnt.Options {
			if strings.HasPrefix(o, rebindInodeOptPrefix) {
				inoOpt = o
			}
		}
		require.NotEmpty(t, inoOpt, "expected an x-podman-dev-ino option")

		// The recorded major:minor:ino must match the real source inode.
		assert.Equal(t, rebindInodeOptPrefix+statxInode(t, src), inoOpt)
		assert.NotContains(t, devMnt.Options, rebindPendingOpt)
	})

	t.Run("records by-id for a serial device", func(t *testing.T) {
		// /dev/ttyACM0 has a /dev/serial/by-id symlink in this environment.
		byId := serialByIdFor(t, "/dev/ttyACM0")
		if byId == "" {
			t.Skip("no /dev/serial/by-id link for /dev/ttyACM0 in this environment")
		}
		devMnt := &spec.Mount{}
		applyRebindOptions("/dev/ttyACM0", devMnt)

		var byIdOpt string
		for _, o := range devMnt.Options {
			if strings.HasPrefix(o, rebindByIdOptPrefix) {
				byIdOpt = strings.TrimPrefix(o, rebindByIdOptPrefix)
			}
		}
		require.NotEmpty(t, byIdOpt, "expected an x-podman-dev-by-id option")
		assert.Equal(t, byId, byIdOpt, "recorded by-id should match the host link")
	})

	t.Run("marks pending when source absent", func(t *testing.T) {
		devMnt := &spec.Mount{}
		applyRebindOptions("/dev/does-not-exist-probe", devMnt)
		assert.Contains(t, devMnt.Options, rebindPendingOpt)
		assert.NotContains(t, devMnt.Options, rebindInodeOptPrefix)
	})
}

// TestDevicesFromPath_rebindRecordsInode exercises the full flag-parsing ->
// addDevice -> inode-recording path and asserts the golden inode is stashed as
// an x- prefixed mount option that runc will preserve into the runtime spec.
func TestDevicesFromPath_rebindRecordsInode(t *testing.T) {
	if !rootless.IsRootless() {
		t.Skip("rootless device mount path only exercised in rootless mode")
	}
	// /dev/null is a stable char device node (major 1, minor 3) whose inode we
	// can predict, so we can assert the recorded option matches it.
	src := "/dev/null"

	gen, err := rtgen.New("linux")
	require.NoError(t, err)
	require.NoError(t, DevicesFromPath(&gen, src+",rebind", &config.Config{}))

	var inoOpt string
	for _, m := range gen.Config.Mounts {
		for _, o := range m.Options {
			if strings.HasPrefix(o, rebindInodeOptPrefix) {
				inoOpt = o
			}
		}
	}
	require.NotEmpty(t, inoOpt, "expected x-podman-dev-ino option on the bind mount")
	assert.Equal(t, rebindInodeOptPrefix+statxInode(t, src), inoOpt)
}
