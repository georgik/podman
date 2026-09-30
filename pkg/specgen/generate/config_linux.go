//go:build !remote

package generate

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"go.podman.io/common/pkg/config"
	"go.podman.io/podman/v6/libpod/define"
	"go.podman.io/podman/v6/pkg/rootless"
	"go.podman.io/podman/v6/pkg/util"
	"go.podman.io/storage/pkg/fileutils"

	spec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
	"tags.cncf.io/container-device-interface/pkg/cdi"
)

// DevicesFromPath computes a list of devices
func DevicesFromPath(g *generate.Generator, devicePath string, config *config.Config) error {
	if isCDIDevice(devicePath) {
		registry, err := cdi.NewCache(
			cdi.WithSpecDirs(config.Engine.CdiSpecDirs.Get()...),
			cdi.WithAutoRefresh(false),
		)
		if err != nil {
			return fmt.Errorf("creating CDI registry: %w", err)
		}
		if err := registry.Refresh(); err != nil {
			logrus.Debugf("The following error was triggered when refreshing the CDI registry: %v", err)
		}
		if _, err := registry.InjectDevices(g.Config, devicePath); err != nil {
			return fmt.Errorf("setting up CDI devices: %w", err)
		}
		return nil
	}
	// A trailing ",rebind"/",persistent" option (comma-separated, e.g.
	// "/dev/ttyACM0,rebind") requests that the mount be kept valid across
	// device re-enumeration (a USB power cycle allocates a new inode). Strip it
	// here so the standard ":"-separated src/dst/mode parsing below is
	// unaffected, and thread the flag into addDevice().
	cleanedDevicePath, rebind := splitDeviceSpec(devicePath)
	devs := strings.Split(cleanedDevicePath, ":")
	resolvedDevicePath := devs[0]
	// check if it is a symbolic link
	if src, err := os.Lstat(resolvedDevicePath); err == nil && src.Mode()&os.ModeSymlink == os.ModeSymlink {
		if linkedPathOnHost, err := filepath.EvalSymlinks(resolvedDevicePath); err == nil {
			resolvedDevicePath = linkedPathOnHost
		}
	}
	st, err := os.Stat(resolvedDevicePath)
	if err != nil {
		return err
	}
	if st.IsDir() {
		found := false
		src := resolvedDevicePath
		dest := src
		var devmode string
		if len(devs) > 1 {
			if len(devs[1]) > 0 && devs[1][0] == '/' {
				dest = devs[1]
			} else {
				devmode = devs[1]
			}
		}
		if len(devs) > 2 {
			if devmode != "" {
				return fmt.Errorf("invalid device specification %s: %w", devicePath, unix.EINVAL)
			}
			devmode = devs[2]
		}

		// mount the internal devices recursively
		if err := filepath.WalkDir(resolvedDevicePath, func(dpath string, d fs.DirEntry, _ error) error {
			if d.Type()&os.ModeDevice == os.ModeDevice {
				found = true
				device := fmt.Sprintf("%s:%s", dpath, filepath.Join(dest, strings.TrimPrefix(dpath, src)))
				if devmode != "" {
					device = fmt.Sprintf("%s:%s", device, devmode)
				}
				if err := addDevice(g, device, false); err != nil {
					return fmt.Errorf("failed to add %s device: %w", dpath, err)
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("no devices found in %s: %w", devicePath, unix.EINVAL)
		}
		return nil
	}
	return addDevice(g, strings.Join(append([]string{resolvedDevicePath}, devs[1:]...), ":"), rebind)
}

func BlockAccessToKernelFilesystems(privileged, pidModeIsHost bool, mask, unmask []string, g *generate.Generator) {
	if !privileged {
		for _, mp := range config.DefaultMaskedPaths() {
			// check that the path to mask is not in the list of paths to unmask
			if shouldMask(mp, unmask) {
				g.AddLinuxMaskedPaths(mp)
			}
		}
		for _, rp := range config.DefaultReadOnlyPaths {
			if shouldMask(rp, unmask) {
				g.AddLinuxReadonlyPaths(rp)
			}
		}

		if pidModeIsHost && rootless.IsRootless() {
			return
		}
	}

	// mask the paths provided by the user
	for _, mp := range mask {
		if !path.IsAbs(mp) && mp != "" {
			logrus.Errorf("Path %q is not an absolute path, skipping...", mp)
			continue
		}
		g.AddLinuxMaskedPaths(mp)
	}
}

func addDevice(g *generate.Generator, device string, rebind bool) error {
	src, dst, permissions, err := ParseDevice(device)
	if err != nil {
		return err
	}
	dev, err := util.DeviceFromPath(src)
	if err != nil {
		return fmt.Errorf("%s is not a valid device: %w", src, err)
	}
	if rootless.IsRootless() {
		perm := "ro"
		if strings.Contains(permissions, "w") {
			perm = "rw"
		}
		devMnt := spec.Mount{
			Destination: dst,
			Type:        define.TypeBind,
			Source:      src,
			Options:     []string{"slave", "nosuid", "noexec", perm, "rbind"},
		}
		// When rebind persistence is requested, record the source inode so
		// that `podman device reconcile` can detect a later re-enumeration
		// (a power cycle allocates a new inode for the device node). The
		// recorded value rides on the existing Mounts list as an x- prefixed
		// option that runc preserves verbatim into the runtime spec.
		if rebind {
			applyRebindOptions(src, &devMnt)
		} else {
			// A non-rebind device must be present at container creation.
			if err := fileutils.Exists(src); err != nil {
				return err
			}
		}
		g.Config.Mounts = append(g.Config.Mounts, devMnt)
		return nil
	} else if src == "/dev/fuse" {
		// if the user is asking for fuse inside the container
		// make sure the module is loaded.
		f, err := unix.Open(src, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err == nil {
			unix.Close(f)
		}
	}
	dev.Path = dst
	g.AddDevice(*dev)
	g.AddLinuxResourcesDevice(true, dev.Type, &dev.Major, &dev.Minor, permissions)
	return nil
}

// Rebind persistence options. These are x- prefixed mount options that runc
// preserves verbatim into the runtime spec (OCI allows unknown x- options), so
// the feature requires no runc / OCI-spec changes.
const (
	// rebindInodeOptPrefix carries the recorded (major,minor):inode pair of the
	// source device at container creation, e.g. "x-podman-dev-ino=189:5".
	rebindInodeOptPrefix = "x-podman-dev-ino="
	// rebindByIdOptPrefix carries the host /dev/serial/by-id/<id> identity of
	// the source device, e.g. "x-podman-dev-by-id=usb-generic-serial-1234-if00".
	// The by-id is the stable identity that survives USB renumbering, so
	// reconcile can identify exactly which physical board a rebind corresponds
	// to and stat the device it currently resolves to (robust to the ttyACM/
	// ttyUSB number changing after a re-enumeration).
	rebindByIdOptPrefix = "x-podman-dev-by-id="
	// rebindPendingOpt marks a rebind device whose source was not present at
	// container creation (not yet plugged in); reconcile skips it until it
	// appears.
	rebindPendingOpt = "x-podman-dev-pending"
)

// serialByIdDir is where the kernel exposes the stable per-device symlinks.
const serialByIdDir = "/dev/serial/by-id"

// splitDeviceSpec recognises the optional trailing ",rebind"/",persistent"
// device option (comma-separated, e.g. "/dev/ttyACM0,rebind") and returns the
// device path with that option stripped together with a boolean reporting
// whether rebind persistence was requested. Any other trailing token is left
// untouched so existing r/w/m device-mode parsing is unaffected.
func splitDeviceSpec(devicePath string) (string, bool) {
	if idx := strings.LastIndexByte(devicePath, ','); idx >= 0 {
		token := strings.ToLower(strings.TrimSpace(devicePath[idx+1:]))
		if token == "rebind" || token == "persistent" {
			return devicePath[:idx], true
		}
	}
	return devicePath, false
}

// applyRebindOptions records the source device's device number and inode so
// that reconcile can detect a later re-enumeration. If the source cannot be
// stat'd (e.g. not plugged in yet) the device is marked pending so that
// container start still succeeds.
func applyRebindOptions(src string, devMnt *spec.Mount) {
	var st unix.Statx_t
	// Request the full statx set so the Rdev_* fields (the device's own
	// major:minor, e.g. 166:1 for a USB serial node) are populated. We record
	// st_rdev, NOT st_dev: st_dev is the filesystem/devtmpfs number (0:7) and
	// is identical for every node on the same tmpfs, so it cannot tell a
	// re-enumerated device apart. st_rdev is the device's real identity and is
	// what reconcile later compares against to detect a re-enumeration.
	if err := unix.Statx(int(unix.AT_FDCWD), src, 0, unix.STATX_ALL, &st); err != nil {
		logrus.Debugf("rebind: could not stat device %q, marking pending: %v", src, err)
		devMnt.Options = append(devMnt.Options, rebindPendingOpt)
		return
	}
	devMnt.Options = append(devMnt.Options,
		fmt.Sprintf("%s%d:%d:%d", rebindInodeOptPrefix, st.Rdev_major, st.Rdev_minor, st.Ino))
	// Record the stable /dev/serial/by-id identity so reconcile can identify
	// exactly which physical board a rebind corresponds to, robust to the tty
	// number changing after a re-enumeration. statx follows the by-id symlink
	// to the current device, so reconcile can stat it directly.
	if byId, ok := serialById(src); ok {
		devMnt.Options = append(devMnt.Options,
			fmt.Sprintf("%s%s", rebindByIdOptPrefix, byId))
	}
}

// serialById returns the /dev/serial/by-id/<id> symlink name that resolves to
// the given device source path, if one exists. It matches on the device's own
// device number (st_rdev) so it finds the by-id link for a /dev/ttyACM* path
// even though the by-id link name does not contain the tty number. The return
// is just the link name (the part after /dev/serial/by-id/); reconcile
// resolves it against the host. A source that is not a USB serial device (no
// matching by-id link) yields ("", false) so the caller simply omits the
// option and reconciles against the source path.
func serialById(src string) (string, bool) {
	entries, err := os.ReadDir(serialByIdDir)
	if err != nil {
		return "", false
	}
	var st unix.Statx_t
	if err := unix.Statx(int(unix.AT_FDCWD), src, 0, unix.STATX_ALL, &st); err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		link := filepath.Join(serialByIdDir, e.Name())
		var linkSt unix.Statx_t
		if err := unix.Statx(int(unix.AT_FDCWD), link, 0, unix.STATX_ALL, &linkSt); err != nil {
			continue
		}
		// Match on the device's own major:minor (rdev), which is unique per
		// connected node, so the by-id link is pinned to exactly this board.
		if linkSt.Rdev_major == st.Rdev_major && linkSt.Rdev_minor == st.Rdev_minor {
			return e.Name(), true
		}
	}
	return "", false
}

func supportAmbientCapabilities() bool {
	err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_IS_SET, 0, 0, 0)
	return err == nil
}

func shouldMask(mask string, unmask []string) bool {
	for _, m := range unmask {
		if strings.EqualFold(m, "all") {
			return false
		}
		for m1 := range strings.SplitSeq(m, ":") {
			match, err := filepath.Match(m1, mask)
			if err != nil {
				logrus.Error(err.Error())
			}
			if match {
				return false
			}
		}
	}
	return true
}
