package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.podman.io/common/pkg/completion"
	"go.podman.io/podman/v6/cmd/podman/common"
	"go.podman.io/podman/v6/cmd/podman/registry"
	"go.podman.io/podman/v6/cmd/podman/validate"
	"go.podman.io/podman/v6/pkg/domain/entities"
)

var (
	// Command: podman _device_
	deviceCmd = &cobra.Command{
		Use:   "device",
		Short: "Manage device mounts",
		Long:  "Manage device mounts, e.g. keep them valid across a power cycle.",
		RunE:  validate.SubCommandExists,
	}

	deviceReconcileDescription = `Rebind device mounts that were recorded with --device <src>,rebind to the
inode that currently lives at their source path.

When a device is power-cycled (USB plug-out / port-power-off) the host allocates a
new inode for it. A normal --device mount is anchored to the inode captured at
container start, so it dangles on the now-dead inode until the container is
restarted. Devices added with the ",rebind" (alias ",persistent") option record
their source inode, and this command re-attaches them to the fresh inode without
restarting the container.

The umount/mount --bind is performed inside the container's own mount namespace
(via an exec session), so the command works from the host against a running
container. The rebind only takes effect while the container is running and needs
CAP_SYS_ADMIN + a mount-permitting seccomp profile (see the
persistent-device-mounts design for details).

Automatic mode: a container started with a ",rebind" device is watched
automatically; the daemon rebinds it the moment the host re-enumerates the
device (typically within a fraction of a second of a power-up), so no manual
command is needed. This one-shot command reconciles immediately instead.`

	reconcileCmd = &cobra.Command{
		Use:               "reconcile <container> [container...]",
		Short:             "Rebind re-enumerated device mounts to their current host inode",
		Long:              deviceReconcileDescription,
		RunE:              reconcile,
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: common.AutocompleteContainers,
		Example: `# reconcile a single container once
podman device reconcile myboard

# reconcile several containers, never failing on a missing one
podman device reconcile --ignore board1 board2

# manually watch a container (the automatic watcher already covers containers
# started with a ,rebind device; this is for ad-hoc / legacy containers)
podman device reconcile --watch --interval 250ms myboard`,
	}

	reconcileOptions entities.ReconcileDevicesOptions
)

func init() {
	reconcileCmd.Flags().BoolVar(&reconcileOptions.Watch, "watch", false, "Reconcile continuously on --interval until interrupted")
	reconcileCmd.Flags().DurationVar(&reconcileOptions.Interval, "interval", 250*time.Millisecond, "Interval between reconcile passes when --watch is set")
	reconcileCmd.Flags().BoolVar(&reconcileOptions.Latest, "latest", false, "Reconcile the most recently created container")
	reconcileCmd.Flags().BoolVar(&reconcileOptions.Ignore, "ignore", false, "Do not fail if a named container does not exist")
	_ = reconcileCmd.RegisterFlagCompletionFunc("interval", completion.AutocompleteDefault)
	// Reconcile performs mount operations, so it only makes sense in local
	// (abi) mode; over the remote API the host would have to remount into the
	// container's namespace, which the tunnel client cannot do.
	reconcileCmd.Annotations = map[string]string{registry.EngineMode: registry.ABIMode}

	registry.Commands = append(registry.Commands, registry.CliCommand{
		Command: reconcileCmd,
		Parent:  deviceCmd,
	})
	registry.Commands = append(registry.Commands, registry.CliCommand{
		Command: deviceCmd,
	})
}

func reconcile(cmd *cobra.Command, args []string) error {
	ctx := registry.Context()
	if reconcileOptions.Watch {
		return reconcileWatch(ctx, args)
	}
	return reconcileOnce(ctx, args)
}

func reconcileOnce(ctx context.Context, args []string) error {
	reports, err := registry.ContainerEngine().ContainerReconcileDevices(ctx, args, reconcileOptions)
	if err != nil {
		return err
	}
	_ = renderReconcile(os.Stdout, reports, false, time.Now())
	return nil
}

func reconcileWatch(ctx context.Context, args []string) error {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	ticker := time.NewTicker(reconcileOptions.Interval)
	defer ticker.Stop()

	first := true
	for {
		select {
		case <-stop:
			fmt.Fprintln(os.Stdout, "stopped")
			return nil
		case <-ticker.C:
			if !first {
				continue
			}
			first = false
			reports, err := registry.ContainerEngine().ContainerReconcileDevices(ctx, args, reconcileOptions)
			if err != nil {
				return err
			}
			// In watch mode only surface devices that actually changed, so the
			// loop stays quiet while the device is stable.
			_ = renderReconcile(os.Stdout, reports, true, time.Now())
		}
	}
}

// renderReconcile writes the reconcile results. When onlyChanges is true it
// prints just the devices that were rebound and reports whether any were.
func renderReconcile(w *os.File, reports []*entities.ReconcileDevicesReport, onlyChanges bool, at time.Time) bool {
	changed := false
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, r := range reports {
		for _, res := range r.Results {
			if onlyChanges && !res.Rebounded {
				continue
			}
			changed = true
			status := "ok"
			switch {
			case res.Err != "":
				status = "error"
			case res.Rebounded:
				status = "rebounded"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				r.ID,
				res.Src,
				res.Dst,
				status,
				res.Err,
			)
		}
	}
	_ = tw.Flush()
	if changed {
		fmt.Fprintf(w, "%s\n", at.UTC().Format(time.RFC3339))
	}
	return changed
}
