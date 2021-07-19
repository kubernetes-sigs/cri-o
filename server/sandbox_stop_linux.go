// +build linux

package server

import (
	"fmt"

	"github.com/containers/storage"
	"github.com/cri-o/cri-o/internal/lib/sandbox"
	"github.com/cri-o/cri-o/internal/log"
	oci "github.com/cri-o/cri-o/internal/oci"
	"github.com/cri-o/cri-o/internal/runtimehandlerhooks"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	"golang.org/x/sync/errgroup"
)

func (s *Server) stopPodSandbox(ctx context.Context, sb *sandbox.Sandbox) error {
	stopMutex := sb.StopMutex()
	stopMutex.Lock()
	defer stopMutex.Unlock()

	// Clean up sandbox networking and close its network namespace.
	if err := s.networkStop(ctx, sb); err != nil {
		return err
	}

	if sb.Stopped() {
		log.Infof(ctx, "Stopped pod sandbox (already stopped): %s", sb.ID())
		return nil
	}

	// Get high-performance runtime hook to trigger preStop step for each container
	hooks, err := runtimehandlerhooks.GetRuntimeHandlerHooks(&s.config, sb.Annotations())
	if err != nil {
		return fmt.Errorf("failed to get runtime handler %q hooks", sb.RuntimeHandler())
	}

	podInfraContainer := sb.InfraContainer()
	containers := sb.Containers().List()
	containers = append(containers, podInfraContainer)

	const maxWorkers = 128
	var waitGroup errgroup.Group
	for i := 0; i < len(containers); i += maxWorkers {
		max := i + maxWorkers
		if len(containers) < max {
			max = len(containers)
		}
		for _, ctr := range containers[i:max] {
			cStatus := ctr.State()
			if cStatus.Status != oci.ContainerStateStopped {
				if ctr.ID() == podInfraContainer.ID() {
					continue
				}
				c := ctr
				waitGroup.Go(func() error {
					if err := s.StopContainerAndWait(ctx, c, int64(10)); err != nil {
						return fmt.Errorf("failed to stop container for pod sandbox %s: %v", sb.ID(), err)
					}
					if err := s.StorageRuntimeServer().StopContainer(c.ID()); err != nil && !errors.Is(err, storage.ErrContainerUnknown) {
						// assume container already umounted
						log.Warnf(ctx, "Failed to stop container %s in pod sandbox %s: %v", c.Name(), sb.ID(), err)
					}
					if err := s.ContainerStateToDisk(ctx, c); err != nil {
						return errors.Wrapf(err, "write container %q state do disk", c.Name())
					}
					return nil
				})
			}
			if hooks != nil {
				if err := hooks.PreStop(ctx, ctr, sb); err != nil {
					log.Warnf(ctx, "Failed to run PreStop hook for container %s in pod sandbox %s: %v", ctr.Name(), sb.ID(), err)
				}
			}
		}
		if err := waitGroup.Wait(); err != nil {
			return err
		}
	}

	if podInfraContainer != nil {
		podInfraStatus := podInfraContainer.State()
		if podInfraStatus.Status != oci.ContainerStateStopped {
			if err := s.StopContainerAndWait(ctx, podInfraContainer, int64(10)); err != nil {
				return fmt.Errorf("failed to stop infra container for pod sandbox %s: %v", sb.ID(), err)
			}
		}
	}

	if err := sb.UnmountShm(); err != nil {
		return err
	}

	if err := s.StorageRuntimeServer().StopContainer(sb.ID()); err != nil && !errors.Is(err, storage.ErrContainerUnknown) {
		log.Warnf(ctx, "Failed to stop sandbox container in pod sandbox %s: %v", sb.ID(), err)
	}
	if err := s.ContainerStateToDisk(ctx, podInfraContainer); err != nil {
		log.Warnf(ctx, "Error writing pod infra container %q state to disk: %v", podInfraContainer.ID(), err)
	}

	log.Infof(ctx, "Stopped pod sandbox: %s", sb.ID())
	sb.SetStopped(true)

	return nil
}
