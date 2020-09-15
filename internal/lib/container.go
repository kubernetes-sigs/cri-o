package lib

import (
	"fmt"

	"github.com/containers/podman/v3/pkg/registrar"
	cstorage "github.com/containers/storage"
	"github.com/cri-o/cri-o/internal/lib/sandbox"
	"github.com/cri-o/cri-o/internal/oci"
)

// GetStorageContainer searches for a container with the given name or ID in the given store
func (c *ContainerServer) GetStorageContainer(container string) (*cstorage.Container, error) {
	ociCtr, err := c.LookupContainer(container)
	if err != nil {
		return nil, err
	}
	return c.store.Container(ociCtr.ID())
}

// GetContainerTopLayerID gets the ID of the top layer of the given container
func (c *ContainerServer) GetContainerTopLayerID(containerID string) (string, error) {
	ctr, err := c.GetStorageContainer(containerID)
	if err != nil {
		return "", err
	}
	return ctr.LayerID, nil
}

// GetContainerFromShortID gets an oci container matching the specified full or partial id
func (c *ContainerServer) GetContainerFromShortID(cid string) (*oci.Container, error) {
	if cid == "" {
		return nil, fmt.Errorf("container ID should not be empty")
	}

	containerID, err := c.ctrIDIndex.Get(cid)
	if err != nil {
		return nil, fmt.Errorf("container with ID starting with %s not found: %v", cid, err)
	}

	ctr := c.GetContainer(containerID)
	if ctr == nil {
		return nil, fmt.Errorf("specified container not found: %s", containerID)
	}

	if !ctr.Created() {
		return nil, fmt.Errorf("specified container %s is not yet created", containerID)
	}

	return ctr, nil
}

// LookupContainer returns the container with the given name or full or partial id
func (c *ContainerServer) LookupContainer(idOrName string) (*oci.Container, error) {
	if idOrName == "" {
		return nil, fmt.Errorf("container ID or name should not be empty")
	}

	ctrID, err := c.ctrNameIndex.Get(idOrName)
	if err != nil {
		if err == registrar.ErrNameNotReserved {
			ctrID = idOrName
		} else {
			return nil, err
		}
	}

	return c.GetContainerFromShortID(ctrID)
}

func (c *ContainerServer) getSandboxFromRequest(pid string) (*sandbox.Sandbox, error) {
	if pid == "" {
		return nil, fmt.Errorf("pod ID should not be empty")
	}

	podID, err := c.podIDIndex.Get(pid)
	if err != nil {
		return nil, fmt.Errorf("pod with ID starting with %s not found: %v", pid, err)
	}

	sb := c.GetSandbox(podID)
	if sb == nil {
		return nil, fmt.Errorf("specified pod not found: %s", podID)
	}
	return sb, nil
}

// LookupSandbox returns the pod sandbox with the given name or full or partial id
func (c *ContainerServer) LookupSandbox(idOrName string) (*sandbox.Sandbox, error) {
	if idOrName == "" {
		return nil, fmt.Errorf("container ID or name should not be empty")
	}

	podID, err := c.podNameIndex.Get(idOrName)
	if err != nil {
		if err == registrar.ErrNameNotReserved {
			podID = idOrName
		} else {
			return nil, err
		}
	}

	return c.getSandboxFromRequest(podID)
}
