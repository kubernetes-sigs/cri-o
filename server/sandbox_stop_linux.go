// +build linux

package server

import (
	"fmt"

	"github.com/containers/storage"
	"github.com/cri-o/cri-o/internal/lib/sandbox"
	"github.com/cri-o/cri-o/internal/log"
	"github.com/cri-o/cri-o/internal/runtimehandlerhooks"
	"github.com/cri-o/cri-o/server/cri/types"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	"golang.org/x/sync/errgroup"
)

func (s *Server) stopPodSandbox(ctx context.Context, req *types.StopPodSandboxRequest) error {
	log.Infof(ctx, "Stopping pod sandbox: %s", req.PodSandboxID)
	sb, err := s.getPodSandboxFromRequest(req.PodSandboxID)
	if err != nil {
		if err == sandbox.ErrIDEmpty {
			return err
		}
		if err == errSandboxNotCreated {
			return fmt.Errorf("StopPodSandbox failed as the sandbox is not created: %s", sb.ID())
		}

		// If the sandbox isn't found we just return an empty response to adhere
		// the CRI interface which expects to not error out in not found
		// cases.

		log.Warnf(ctx, "could not get sandbox %s, it's probably been stopped already: %v", req.PodSandboxID, err)
		log.Debugf(ctx, "StopPodSandboxResponse %s", req.PodSandboxID)
		return nil
	}
	stopMutex := sb.StopMutex()
	stopMutex.Lock()
	defer stopMutex.Unlock()

	// Clean up sandbox networking and close its network namespace.
	if err := s.networkStop(ctx, sb); err != nil {
		return err
	}

	// Get high-performance runtime hook to trigger preStop step for each container
	hooks, err := runtimehandlerhooks.GetRuntimeHandlerHooks(ctx, &s.config, sb.RuntimeHandler(), s.Runtime())
	if err != nil {
		return fmt.Errorf("failed to get runtime handler %q hooks", sb.RuntimeHandler())
	}

	if sb.Stopped() {
		log.Infof(ctx, "Stopped pod sandbox (already stopped): %s", sb.ID())
		return nil
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
			if ctr.IsAlive() == nil {
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
						log.Warnf(ctx, "failed to stop container %s in pod sandbox %s: %v", c.Name(), sb.ID(), err)
					}
					if err := s.ContainerStateToDisk(c); err != nil {
						return errors.Wrapf(err, "write container %q state do disk", c.Name())
					}
					return nil
				})
			}
			if hooks != nil {
				if err := hooks.PreStop(ctx, ctr, sb); err != nil {
					log.Warnf(ctx, "failed to run PreStop hook for container %s in pod sandbox %s: %v", ctr.Name(), sb.ID(), err)
				}
			}
		}
		if err := waitGroup.Wait(); err != nil {
			return err
		}
	}

	// only stop the infra container if it exists and is still alive
	if podInfraContainer != nil && !podInfraContainer.Spoofed() && podInfraContainer.IsAlive() == nil {
		if err := s.StopContainerAndWait(ctx, podInfraContainer, int64(10)); err != nil {
			return fmt.Errorf("failed to stop infra container for pod sandbox %s: %v", sb.ID(), err)
		}
	}

	if err := sb.UnmountShm(); err != nil {
		return err
	}

	if err := s.StorageRuntimeServer().StopContainer(sb.ID()); err != nil && !errors.Is(err, storage.ErrContainerUnknown) {
		log.Warnf(ctx, "failed to stop sandbox container in pod sandbox %s: %v", sb.ID(), err)
	}
	if err := s.ContainerStateToDisk(podInfraContainer); err != nil {
		log.Warnf(ctx, "error writing pod infra container %q state to disk: %v", podInfraContainer.ID(), err)
	}

	log.Infof(ctx, "Stopped pod sandbox: %s", sb.ID())
	sb.SetStopped(true)

	return nil
}
