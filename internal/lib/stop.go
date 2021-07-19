package lib

import (
	"context"

	"github.com/cri-o/cri-o/internal/oci"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// ContainerStop stops a running container with a grace period (i.e., timeout).
func (c *ContainerServer) StopContainer(ctx context.Context, ctr *oci.Container, timeout int64) error {
	if err := c.runtime.StopContainer(ctx, ctr, timeout); err != nil {
		// only fatally error if the error is not that the container was already stopped
		// we still want to write container state to disk if the container has already
		// been stopped
		if err != oci.ErrContainerStopped {
			return errors.Wrapf(err, "failed to stop container %s", ctr.ID())
		}
	} else {
		// we only do these operations if StopContainer didn't fail (even if the failure
		// was the container already being stopped)
		if err := c.runtime.WaitContainerStateStopped(ctx, ctr); err != nil {
			return errors.Wrapf(err, "failed to get container 'stopped' status %s", ctr.ID())
		}
		if err := c.storageRuntimeServer.StopContainer(ctr.ID()); err != nil {
			return errors.Wrapf(err, "failed to unmount container %s", ctr.ID())
		}
	}

	if err := c.ContainerStateToDisk(ctr); err != nil {
		logrus.Warnf("unable to write containers %s state to disk: %v", ctr.ID(), err)
	}

	return nil
}
