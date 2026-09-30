//go:build !remote

package libpod

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	spec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"
	"go.podman.io/podman/v6/libpod/define"
	"golang.org/x/sys/unix"
)

// rebindInodeOptPrefix / rebindPendingOpt mirror the constants in
// pkg/specgen/generate/config_linux.go. They are x- prefixed mount options that
// runc preserves verbatim into the runtime spec, so the recorded inode travels
// from container creation to reconcile without any runc / OCI-spec change.
const (
	rebindInodeOptPrefix = "x-podman-dev-ino="
	// rebindByIdOptPrefix carries the host /dev/serial/by-id/<id> identity of
	// the source device, e.g. "x-podman-dev-by-id=usb-generic-serial-1234-if00".
	// The by-id is the stable identity that survives USB renumbering, so it
	// lets reconcile identify exactly which physical board a rebind corresponds
	// to and stat the device it currently resolves to (robust to the ttyACM/
	// ttyUSB number changing after a re-enumeration).
	rebindByIdOptPrefix = "x-podman-dev-by-id="
	rebindPendingOpt    = "x-podman-dev-pending"
)

// rebindSerialByIdDir is where the kernel exposes the stable per-device
// symlinks. The recorded x-podman-dev-by-id value is the link name only (the
// identity), so reconcile joins it with this directory to stat the device.
const rebindSerialByIdDir = "/dev/serial/by-id"

// recordedInode is the (device number, inode) pair captured at container
// creation for a rebind device. Any divergence from the current statx of the
// source path means the device was re-enumerated (e.g. a USB power cycle).
type recordedInode struct {
	Major uint32
	Minor uint32
	Ino   uint64
}

func (i recordedInode) String() string {
	return fmt.Sprintf("%d:%d:%d", i.Major, i.Minor, i.Ino)
}

// DeviceReconcileResult reports the outcome of reconciling a single rebind device mount.
type DeviceReconcileResult struct {
	Src       string
	Dst       string
	Pending   bool
	Rebounded bool
	Err       error
}

// rebindHostDevDir is the path inside the container where the host's /dev is
// bind-mounted (see the container launch: --volume=/dev:/hostdev:ro). It is the
// source the rebind rebinds from: a bind of this directory reflects the current
// host inode, so it picks up the re-enumerated device after a power cycle.
const rebindHostDevDir = "/hostdev"

// umountFn / mountBindFn are indirections so tests can stub the rebind
// operations. In production they drive util-linux's umount and mount --bind.
// The rebind rebinds the container's device node to the host's current inode
// (see reconcileDeviceMount), which is what lets a power-cycled device come
// back without restarting the container.
var (
	umountFn = func(dst string) error {
		out, err := exec.Command("umount", dst).CombinedOutput()
		if err != nil {
			return fmt.Errorf("umount %q: %w: %s", dst, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	// mountBindFn binds the host's current device (reachable via
	// rebindHostDevDir, the host /dev mounted into the container) onto the
	// container's device node. Because a bind reflects the inode that lives at
	// the source path at mount time, it picks up the freshly re-enumerated host
	// inode after a power cycle.
	mountBindFn = func(dst, hostDev string) error {
		src := filepath.Join(rebindHostDevDir, hostDev)
		out, err := exec.Command("mount", "--bind", src, dst).CombinedOutput()
		if err != nil {
			return fmt.Errorf("mount --bind %q %q: %w: %s", src, dst, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
)

// parseRebindOpt extracts the recorded inodes, the stable /dev/serial/by-id
// identity, and the pending flag from a mount's option list. It recognises:
//
//	x-podman-dev-ino=<major>:<minor>:<ino>   (one or more)
//	x-podman-dev-by-id=<by-id-string>       (the host serial/by-id link name)
//	x-podman-dev-pending
//
// Malformed entries are ignored (and logged via the returned parse) rather
// than aborting the whole reconcile. The by-id is recorded once (the first
// well-formed value wins) because it is a single stable identity per mount.
func parseRebindOpt(options []string) (inodes []recordedInode, byId string, pending bool) {
	for _, o := range options {
		switch {
		case strings.HasPrefix(o, rebindInodeOptPrefix):
			_, rest, ok := strings.Cut(o, rebindInodeOptPrefix)
			if !ok {
				continue
			}
			parts := strings.Split(rest, ":")
			if len(parts) != 3 {
				continue
			}
			major, err1 := strconv.ParseUint(parts[0], 10, 32)
			minor, err2 := strconv.ParseUint(parts[1], 10, 32)
			ino, err3 := strconv.ParseUint(parts[2], 10, 64)
			if err1 != nil || err2 != nil || err3 != nil {
				continue
			}
			inodes = append(inodes, recordedInode{Major: uint32(major), Minor: uint32(minor), Ino: ino})
		case strings.HasPrefix(o, rebindByIdOptPrefix):
			// The by-id string may itself contain colons (e.g. the MAC in the
			// link name), so take everything after the prefix verbatim.
			_, id, ok := strings.Cut(o, rebindByIdOptPrefix)
			if ok && id != "" && byId == "" {
				byId = id
			}
		case o == rebindPendingOpt:
			pending = true
		}
	}
	return inodes, byId, pending
}

// rebindStatPath returns the path that reconcile should stat to find the
// device that currently lives at a rebind mount: the stable /dev/serial/by-id
// link when one was recorded, else the recorded source path. statx follows
// the by-id symlink to the current device, so reconciling against it is robust
// to the ttyACM/ttyUSB number changing after a USB re-enumeration.
func rebindStatPath(src, byId string) string {
	if byId != "" {
		// The recorded by-id is the link name only; join it with the base
		// directory to get the path to stat.
		return filepath.Join(rebindSerialByIdDir, byId)
	}
	return src
}

// statSource returns the current (device number, inode) of the source path, or
// an error if the source is not present right now. The device number is read
// from st_rdev (the device's own major:minor, e.g. 166:1 for a USB serial
// node), NOT from st_dev (the filesystem/devtmpfs number, e.g. 0:7) — the
// former is the stable identity we record and rebind from, the latter
// changes with the mount and is useless for that purpose.
func statSource(src string) (recordedInode, error) {
	var st unix.Statx_t
	if err := unix.Statx(int(unix.AT_FDCWD), src, 0, unix.STATX_ALL, &st); err != nil {
		return recordedInode{}, err
	}
	return recordedInode{Major: st.Rdev_major, Minor: st.Rdev_minor, Ino: st.Ino}, nil
}

// reconcileDeviceMount rebinds the device node at dst so it refers to the
// device that currently lives at its source path. It returns (rebounded, err):
//
//   - if the source is absent now, it is a no-op (leave for the next pass);
//   - if the device number and inode are unchanged, it is a no-op (idempotent);
//   - otherwise it umounts the stale bind at <dst> and rebinds the host's
//     current device from rebindHostDevDir so it refers to the current host.
func reconcileDeviceMount(src, dst, byId string, want recordedInode) (rebounded bool, err error) {
	// Stat the stable identity: the /dev/serial/by-id link when recorded, else
	// the source path. statx follows the by-id symlink to the current device,
	// so this is robust to the ttyACM/ttyUSB number changing after a
	// re-enumeration.
	got, serr := statSource(rebindStatPath(src, byId))
	if serr != nil {
		// Source is gone right now (still plugged in?); nothing to reconcile.
		return false, nil
	}
	if got == want {
		// The mount already points at the current inode.
		return false, nil
	}

	// Re-enumerated: detach the stale bind and rebind to the host's current
	// device. We rebind (rather than recreate the node with mknod) because a
	// rootless user namespace rejects CAP_MKNOD, so mknod is unavailable; a
	// bind of the host /dev directory, however, reflects the current host inode
	// and needs only CAP_SYS_ADMIN.
	if uerr := umountFn(dst); uerr != nil {
		// A not-mounted destination is not an error here (the stale mount was
		// already gone); anything else is fatal for this device.
		if !strings.Contains(uerr.Error(), "not mounted") {
			return false, fmt.Errorf("reconcile: umount %q: %w", dst, uerr)
		}
	}
	// Rebind <dst> to the host device reachable via rebindHostDevDir using the
	// host device's own name (basename of the recorded source). The bind of
	// the host directory reflects the current host inode, so the container node
	// refers to the board that is now connected even if the kernel renumbered
	// it after re-enumeration.
	if merr := mountBindFn(dst, filepath.Base(src)); merr != nil {
		return false, fmt.Errorf("reconcile: mount --bind %q: %w", dst, merr)
	}
	return true, nil
}

// reconcileDeviceMounts reconciles every rebind device mount in the given
// mount list. Non-rebind mounts are ignored.
func reconcileDeviceMounts(mounts []spec.Mount) []DeviceReconcileResult {
	results := make([]DeviceReconcileResult, 0, len(mounts))
	for _, m := range mounts {
		inodes, byId, pending := parseRebindOpt(m.Options)
		if pending && len(inodes) == 0 {
			// Source was absent at container start; nothing to reconcile yet.
			continue
		}
		for _, want := range inodes {
			rebounded, rerr := reconcileDeviceMount(m.Source, m.Destination, byId, want)
			results = append(results, DeviceReconcileResult{
				Src:       m.Source,
				Dst:       m.Destination,
				Pending:   pending,
				Rebounded: rebounded,
				Err:       rerr,
			})
		}
	}
	return results
}

// rebindMount describes a single rebind device mount recorded in a container's
// runtime spec: the host source path, the in-container destination, the stable
// /dev/serial/by-id link name (empty when the source is not a USB serial
// device with a by-id link), and the inode that was live at the source when
// the container was created.
type rebindMount struct {
	src  string
	dst  string
	byId string
	want recordedInode
}

// rebindDeviceMounts returns every rebind device mount recorded in the
// container's runtime spec. Non-rebind mounts are ignored.
func (c *Container) rebindDeviceMounts() ([]rebindMount, error) {
	s, err := c.specFromState()
	if err != nil {
		return nil, fmt.Errorf("reconcile: reading container %s spec: %w", c.ID(), err)
	}
	if s == nil {
		return nil, fmt.Errorf("reconcile: container %s has no runtime spec", c.ID())
	}
	mounts := parseRebindMounts(s.Mounts)
	logrus.Debugf("reconcile: container %s specFromState: %d rebind mount(s) from %d spec mounts", c.ID(), len(mounts), len(s.Mounts))
	return mounts, nil
}

// parseRebindMounts returns the rebind mounts recorded in a list of spec
// mounts. Non-rebind mounts are ignored. Extracted from rebindDeviceMounts so
// the parsing is unit-testable without a live container.
func parseRebindMounts(mounts []spec.Mount) []rebindMount {
	var out []rebindMount
	for _, m := range mounts {
		inodes, byId, pending := parseRebindOpt(m.Options)
		if pending && len(inodes) == 0 {
			// Source was absent at container start; nothing to reconcile yet.
			continue
		}
		for _, want := range inodes {
			out = append(out, rebindMount{src: m.Source, dst: m.Destination, byId: byId, want: want})
		}
	}
	return out
}

// rebindViaExec rebinds a rebind device node inside the container's own mount
// namespace via a lightweight exec session, so a power-cycled device comes
// back without restarting the container. It umounts the stale bind and rebinds
// the host's current device from rebindHostDevDir (the host /dev bind-mounted
// into the container); a bind of the host directory reflects the current host
// inode, so the node refers to the re-enumerated board.
//
// Running the umount/mount --bind in the daemon's namespace (e.g. invoking
// `podman device reconcile` from the host) would touch the host node instead of
// the container's mount and silently do nothing.
//
// umount + mount --bind need CAP_SYS_ADMIN in the container's user namespace,
// which a rootless container has only when launched with
// --cap-add=SYS_ADMIN and a mount-permitting seccomp profile (e.g.
// --security-opt=seccomp=unconfined). The host /dev must also be mounted at
// rebindHostDevDir so the fresh inode is reachable from inside.
func (c *Container) rebindViaExec(dst string, hostDev string) error {
	// Run the rebind snippet inside the container's mount namespace so the
	// umount/mount --bind land on the container's device node, not the host's.
	script := rebindScript(dst, hostDev)
	var out, errBuf bytes.Buffer
	config := &ExecConfig{
		Command: []string{"sh", "-c", script},
		// mount()/umount() need CAP_SYS_ADMIN in the container's user
		// namespace, which only uid 0 holds here — the default mapped user
		// would lack the capability.
		User: "0",
	}
	// Attach both stdout and stderr: conmon opens the stderr pipe up front and
	// sends a stderr attach message even when the command writes nothing, so a
	// nil error stream makes the attach fail with "output destination cannot be
	// nil". Capture stderr too so a failing mount is reported verbatim.
	streams := &define.AttachStreams{
		OutputStream: &out,
		ErrorStream:  &errBuf,
		AttachOutput: true,
		AttachError:  true,
	}
	exitCode, err := c.ExecNoSession(config, streams, nil)
	if err != nil {
		return fmt.Errorf("reconcile: exec rebind %q: %w", dst, err)
	}
	if exitCode != 0 {
		rebindOut := strings.TrimSpace(string(out.Bytes()) + string(errBuf.Bytes()))
		return fmt.Errorf("reconcile: rebind %q exited %d: %s", dst, exitCode, rebindOut)
	}
	return nil
}

// rebindScript returns the shell snippet that rebinds a rebind device node
// inside the container's mount namespace. The stale bind is umounted from
// <dst> first (a bind mount cannot be replaced in place); then the host's
// current device is rebound from rebindHostDevDir/<hostDev> (the host /dev
// bind-mounted into the container) so it refers to the current host inode
// after a re-enumeration, without needing CAP_MKNOD.
func rebindScript(dst, hostDev string) string {
	// umount the stale bind first (best effort; a not-mounted destination is
	// not an error). Then mount --bind is the last command, so the script's
	// exit code reflects whether the rebind actually succeeded (rebindViaExec
	// treats a non-zero exit as a failure to retry). The host device is bound
	// from rebindHostDevDir/<hostDev>, the host /dev bind-mounted into the
	// container, so it reflects the current host inode after a re-enumeration.
	// chmod is intentionally omitted: the re-mounted node inherits the host
	// node's mode (already 0666 on the host for these devices).
	return fmt.Sprintf("umount %q 2>/dev/null; mount --bind %q %q",
		dst, filepath.Join(rebindHostDevDir, hostDev), dst)
}

// reconcileOneRebind rebinds a single rebind mount's device node so it
// refers to the device that currently lives at its host source path, performing
// the umount/mount --bind inside the container's own mount namespace (see
// rebindViaExec). A source that has not diverged, or that is absent right now,
// is a no-op. It returns whether a rebind happened.
func (c *Container) reconcileOneRebind(m rebindMount) (bool, error) {
	// Stat the stable identity (the by-id link when recorded, else the source)
	// so a re-enumerated board is detected even if the kernel renumbered the
	// tty. A source that is absent right now is not "needs rebind" (leave it
	// for the next pass).
	got, err := statSource(rebindStatPath(m.src, m.byId))
	if err != nil {
		return false, nil
	}
	if got == m.want {
		// The mount already points at the current inode.
		return false, nil
	}
	// Re-enumerated: rebind to the host device's CURRENT name (resolved from
	// the by-id link when recorded, so a renumbered tty is followed) via
	// rebindHostDevDir, so the node refers to the board now connected.
	devName, err := resolveRebindDevName(m)
	if err != nil {
		return false, nil
	}
	return true, c.rebindViaExec(m.dst, devName)
}

// ReconcileDevices reads this container's runtime spec and recreates every
// rebind device mount's node so it refers to the device that currently lives at
// its source path. The umount/mount --bind are performed inside the
// container's own mount namespace (see rebindViaExec), so a re-enumerated
// device (e.g. after a USB power cycle) is rebound without restarting the
// container. It returns one
// result per recorded rebind device; a nil Err means the device was either
// already up to date or was rebound during this call.
func (c *Container) ReconcileDevices(_ context.Context) ([]DeviceReconcileResult, error) {
	mounts, err := c.rebindDeviceMounts()
	if err != nil {
		return nil, err
	}
	results := make([]DeviceReconcileResult, 0, len(mounts))
	for _, m := range mounts {
		rebounded, rerr := c.reconcileOneRebind(m)
		results = append(results, DeviceReconcileResult{
			Src:       m.src,
			Dst:       m.dst,
			Pending:   false,
			Rebounded: rebounded,
			Err:       rerr,
		})
	}
	return results, nil
}
