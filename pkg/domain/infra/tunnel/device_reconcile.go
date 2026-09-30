package tunnel

import (
	"context"
	"errors"

	"go.podman.io/podman/v6/pkg/domain/entities"
)

// ContainerReconcileDevices reconciles rebind device mounts. The umount/mount
// --bind calls must run inside the container's mount namespace, which a remote
// (tunnel) client cannot do, so it is only supported in local (abi) mode.
func (ic *ContainerEngine) ContainerReconcileDevices(_ context.Context, _ []string, _ entities.ReconcileDevicesOptions) ([]*entities.ReconcileDevicesReport, error) {
	return nil, errors.New("reconcile devices is only supported in local mode, not over the remote API")
}
