//go:build !remote

package libpod

import (
	"path/filepath"
	"strings"
	"testing"

	rspec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseRebindMounts exercises the spec-mount parser shared by the manual
// reconcile path and the global rebind monitor.
func TestParseRebindMounts(t *testing.T) {
	mounts := parseRebindMounts([]rspec.Mount{
		// A rebind device.
		{Source: "/dev/ttyACM0", Destination: "/dev/ttyACM0", Options: []string{"rw", "rbind", "x-podman-dev-ino=166:0:4242"}},
		// A plain (non-rebind) device: ignored.
		{Source: "/dev/ttyACM1", Destination: "/dev/ttyACM1", Options: []string{"rw", "rbind"}},
		// A pending rebind (source absent at start): skipped until it appears.
		{Source: "/dev/ttyACM2", Destination: "/dev/ttyACM2", Options: []string{"rw", "rbind", "x-podman-dev-pending"}},
		// A second rebind device.
		{Source: "/dev/bus/usb/003/005", Destination: "/dev/bus/usb/003/005", Options: []string{"x-podman-dev-ino=189:260:99"}},
	})

	require.Len(t, mounts, 2)
	assert.Equal(t, rebindMount{src: "/dev/ttyACM0", dst: "/dev/ttyACM0", want: recordedInode{166, 0, 4242}}, mounts[0])
	assert.Equal(t, rebindMount{src: "/dev/bus/usb/003/005", dst: "/dev/bus/usb/003/005", want: recordedInode{189, 260, 99}}, mounts[1])
}

// TestRebindScript pins the rebind snippet: umount precedes mount --bind, the
// destination is umounted, and the host device is rebound from /hostdev/<name>.
func TestRebindScript(t *testing.T) {
	same := rebindScript("/dev/ttyACM0", "ttyACM0")
	assert.True(t, strings.Index(same, "umount") < strings.Index(same, "mount --bind"),
		"umount must precede the mount --bind in %q", same)
	assert.Contains(t, same, "mount --bind")
	assert.Contains(t, same, "/dev/ttyACM0")
	// The host device is rebound from the host /dev mounted at /hostdev.
	assert.Contains(t, same, "/hostdev/ttyACM0")

	// The umount must target the *destination* (the mount point inside the
	// container), and the host device must be rebound from /hostdev/<name>.
	diff := rebindScript("/dev/serial/board", "board")
	require.Contains(t, diff, "umount \"/dev/serial/board\" 2>/dev/null")
	require.Contains(t, diff, "mount --bind \"/hostdev/board\" \"/dev/serial/board\"")
	require.NotContains(t, diff, "umount \"/dev/ttyACM0\"")
}

func TestRebindStatPath(t *testing.T) {
	// With no by-id recorded, reconcile stats the source path verbatim.
	assert.Equal(t, "/dev/ttyACM0", rebindStatPath("/dev/ttyACM0", ""))

	// When a stable /dev/serial/by-id link was recorded, reconcile stats that
	// link (joined with the base directory) instead, so a re-enumerated board
	// is found even if the kernel renumbered the tty (ttyACM0 -> ttyUSB1, etc.).
	assert.Equal(t, filepath.Join(rebindSerialByIdDir, "usb-generic-serial-0001-if00"),
		rebindStatPath("/dev/ttyACM0", "usb-generic-serial-0001-if00"))
}

// TestResolveRebindDevName verifies the current-host-device-name resolution the
// monitor and manual reconcile use, so a renumbered tty is followed.
func TestResolveRebindDevName(t *testing.T) {
	// No by-id recorded: fall back to the basename of the source path.
	name, err := resolveRebindDevName(rebindMount{src: "/dev/ttyACM0"})
	require.NoError(t, err)
	assert.Equal(t, "ttyACM0", name)

	// A by-id link resolves to the device's CURRENT name, whatever it is right
	// now (robust to ttyACM0 -> ttyUSB0 after a re-enumeration).
	byId := serialByIdProbeName(t)
	if byId == "" {
		t.Skip("no /dev/serial/by-id char-device link available in this environment")
	}
	name, err = resolveRebindDevName(rebindMount{src: "/dev/ttyACM0", byId: byId})
	require.NoError(t, err)
	// The resolved name is the final component of the (possibly multi-hop) link.
	assert.Equal(t, filepath.Base(name), name)
}
