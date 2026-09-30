//go:build !remote && (linux || freebsd)

package abi

import (
	"context"

	"go.podman.io/podman/v6/pkg/domain/entities"
)

// ContainerReconcileDevices reconciles the rebind device mounts of the named
// containers. Each container's runtime spec is read and every device recorded
// with --device <src>,rebind is checked against the inode that currently lives
// at its source path; a re-enumerated device (e.g. after a USB power cycle) is
// rebound to the fresh inode.
//
// The umount/mount --bind calls land in the namespace of the process running
// this method, so to affect a running container it must be invoked from within
// that container's mount namespace (e.g. via `podman exec` or a prestart hook)
// — see libpod.Container.ReconcileDevices.
func (ic *ContainerEngine) ContainerReconcileDevices(ctx context.Context, namesOrIds []string, opts entities.ReconcileDevicesOptions) ([]*entities.ReconcileDevicesReport, error) {
	ctrs, err := getContainers(ic.Libpod, getContainersOptions{latest: opts.Latest, ignore: opts.Ignore, names: namesOrIds})
	if err != nil {
		return nil, err
	}
	results := make([]*entities.ReconcileDevicesReport, 0, len(ctrs))
	for _, c := range ctrs {
		if c.doesNotExist {
			// Only present when opts.Ignore is set; skip it.
			continue
		}
		res, err := c.ReconcileDevices(ctx)
		if err != nil {
			return nil, err
		}
		report := &entities.ReconcileDevicesReport{
			ID:      c.ID(),
			Name:    c.Name(),
			Results: make([]entities.ReconcileDeviceReport, 0, len(res)),
		}
		for _, r := range res {
			report.Results = append(report.Results, entities.ReconcileDeviceReport{
				Src:       r.Src,
				Dst:       r.Dst,
				Pending:   r.Pending,
				Rebounded: r.Rebounded,
				Err:       errorToString(r.Err),
			})
		}
		results = append(results, report)
	}
	return results, nil
}

func errorToString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
