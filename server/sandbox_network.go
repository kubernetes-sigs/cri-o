package server

import (
	"context"
	"fmt"
	"math"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	cnicurrent "github.com/containernetworking/cni/pkg/types/current"
	"github.com/cri-o/cri-o/internal/hostport"
	"github.com/cri-o/cri-o/internal/lib/sandbox"
	"github.com/cri-o/cri-o/internal/log"
	"github.com/cri-o/cri-o/server/metrics"
	"github.com/cri-o/ocicni/pkg/ocicni"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/api/resource"

	utilnet "k8s.io/utils/net"
)

// networkStart sets up the sandbox's network and returns the pod IP on success
// or an error
func (s *Server) networkStart(ctx context.Context, sb *sandbox.Sandbox) (podIPs []string, result cnitypes.Result, retErr error) {
	overallStart := time.Now()
	// Give a network Start call a full 5 minutes, independent of the context of the request.
	// This is to prevent the CNI plugin from taking an unbounded amount of time,
	// but to still allow a long-running sandbox creation to be cached and reused,
	// rather than failing and recreating it.
	const startTimeout = 5 * time.Minute
	startCtx, startCancel := context.WithTimeout(context.Background(), startTimeout)
	defer startCancel()

	if sb.HostNetwork() {
		return nil, nil, nil
	}

	podNetwork, err := s.newPodNetwork(sb)
	if err != nil {
		return nil, nil, err
	}

	// Ensure network resources are cleaned up if the plugin succeeded
	// but an error happened between plugin success and the end of networkStart()
	defer func() {
		if retErr != nil {
			log.Infof(ctx, "networkStart: stopping network for sandbox %s", sb.ID())
			// use a new context to prevent an expired context from preventing a stop
			if err2 := s.networkStop(context.Background(), sb); err2 != nil {
				log.Errorf(ctx, "error stopping network on cleanup: %v", err2)
			}
		}
	}()

	podSetUpStart := time.Now()
	_, err = s.config.CNIPlugin().SetUpPodWithContext(startCtx, podNetwork)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create pod network sandbox %s(%s): %v", sb.Name(), sb.ID(), err)
	}
	// metric about the CNI network setup operation
	metrics.CRIOOperationsLatency.WithLabelValues("network_setup_pod").
		Observe(metrics.SinceInMicroseconds(podSetUpStart))

	podNetworkStatus, err := s.config.CNIPlugin().GetPodNetworkStatusWithContext(startCtx, podNetwork)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get network status for pod sandbox %s(%s): %v", sb.Name(), sb.ID(), err)
	}

	// only one cnitypes.Result is returned since newPodNetwork sets Networks list empty
	result = podNetworkStatus[0].Result
	log.Debugf(ctx, "CNI setup result: %v", result)

	network, err := cnicurrent.GetResult(result)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get network JSON for pod sandbox %s(%s): %v", sb.Name(), sb.ID(), err)
	}

	// only do portmapping to the first IP of each IP family
	foundIPv4 := false
	foundIPv6 := false
	// cache the portmapping info
	sbID := sb.ID()
	sbName := sb.Name()
	sbPortMappings := sb.PortMappings()
	// iterate over each IP and add the portmap if needed
	for _, podIPConfig := range network.IPs {
		ip := podIPConfig.Address.IP
		podIPs = append(podIPs, ip.String())

		// the pod has host-ports defined
		if len(sbPortMappings) > 0 {
			mapping := &hostport.PodPortMapping{
				Name:         sbName,
				PortMappings: sbPortMappings,
				IP:           ip,
				HostNetwork:  false,
			}
			// nolint:gocritic // using a switch statement is not much different
			if utilnet.IsIPv6(ip) {
				if foundIPv6 {
					// we have already done the portmap for IPv6
					continue
				}
				// found a new IPv6 address, do the portmap
				foundIPv6 = true
			} else if foundIPv4 {
				// we have already done the portmap for IPv4
				continue
			} else {
				// found a new IPv4 address, do the portmap
				foundIPv4 = true
			}
			err = s.hostportManager.Add(sbID, mapping, "")
			if err != nil {
				return nil, nil, fmt.Errorf("failed to add hostport mapping for sandbox %s(%s): %v", sb.Name(), sb.ID(), err)
			}
		}
	}

	log.Debugf(ctx, "found POD IPs: %v", podIPs)

	// metric about the whole network setup operation
	metrics.CRIOOperationsLatency.WithLabelValues("network_setup_overall").
		Observe(metrics.SinceInMicroseconds(overallStart))
	return podIPs, result, err
}

// getSandboxIP retrieves the IP address for the sandbox
func (s *Server) getSandboxIPs(sb *sandbox.Sandbox) ([]string, error) {
	if sb.HostNetwork() {
		return nil, nil
	}

	podNetwork, err := s.newPodNetwork(sb)
	if err != nil {
		return nil, err
	}
	podNetworkStatus, err := s.config.CNIPlugin().GetPodNetworkStatus(podNetwork)
	if err != nil {
		return nil, fmt.Errorf("failed to get network status for pod sandbox %s(%s): %v", sb.Name(), sb.ID(), err)
	}

	res, err := cnicurrent.GetResult(podNetworkStatus[0].Result)
	if err != nil {
		return nil, fmt.Errorf("failed to get network JSON for pod sandbox %s(%s): %v", sb.Name(), sb.ID(), err)
	}

	podIPs := make([]string, 0, len(res.IPs))
	for _, podIPConfig := range res.IPs {
		podIPs = append(podIPs, podIPConfig.Address.IP.String())
	}

	return podIPs, nil
}

// networkStop cleans up and removes a pod's network.  It is best-effort and
// must call the network plugin even if the network namespace is already gone
func (s *Server) networkStop(ctx context.Context, sb *sandbox.Sandbox) error {
	if sb.HostNetwork() || sb.NetworkStopped() {
		return nil
	}
	// give a network stop call 1 minutes, half of a StopPod request timeout limit
	stopCtx, stopCancel := context.WithTimeout(ctx, 1*time.Minute)
	defer stopCancel()

	mapping := &hostport.PodPortMapping{
		Name:         sb.Name(),
		PortMappings: sb.PortMappings(),
		HostNetwork:  false,
	}
	// portMapping removal does not need the IP address
	if err := s.hostportManager.Remove(sb.ID(), mapping); err != nil {
		log.Warnf(ctx, "failed to remove hostport for pod sandbox %s(%s): %v",
			sb.Name(), sb.ID(), err)
	}

	podNetwork, err := s.newPodNetwork(sb)
	if err != nil {
		return err
	}
	if err := s.config.CNIPlugin().TearDownPodWithContext(stopCtx, podNetwork); err != nil {
		return errors.Wrapf(err, "failed to destroy network for pod sandbox %s(%s)", sb.Name(), sb.ID())
	}

	return sb.SetNetworkStopped(true)
}

func (s *Server) newPodNetwork(sb *sandbox.Sandbox) (ocicni.PodNetwork, error) {
	var egress, ingress int64 = 0, 0

	if val, ok := sb.Annotations()["kubernetes.io/egress-bandwidth"]; ok {
		egressQ, err := resource.ParseQuantity(val)
		if err != nil {
			return ocicni.PodNetwork{}, fmt.Errorf("failed to parse egress bandwidth: %v", err)
		} else if iegress, isok := egressQ.AsInt64(); isok {
			egress = iegress
		}
	}
	if val, ok := sb.Annotations()["kubernetes.io/ingress-bandwidth"]; ok {
		ingressQ, err := resource.ParseQuantity(val)
		if err != nil {
			return ocicni.PodNetwork{}, fmt.Errorf("failed to parse ingress bandwidth: %v", err)
		} else if iingress, isok := ingressQ.AsInt64(); isok {
			ingress = iingress
		}
	}

	var bwConfig *ocicni.BandwidthConfig

	if ingress > 0 || egress > 0 {
		bwConfig = &ocicni.BandwidthConfig{}
		if ingress > 0 {
			bwConfig.IngressRate = uint64(ingress)
			bwConfig.IngressBurst = math.MaxUint32 * 8 // 4GB burst limit
		}
		if egress > 0 {
			bwConfig.EgressRate = uint64(egress)
			bwConfig.EgressBurst = math.MaxUint32 * 8 // 4GB burst limit
		}
	}

	network := s.config.CNIPlugin().GetDefaultNetworkName()
	return ocicni.PodNetwork{
		Name:      sb.KubeName(),
		Namespace: sb.Namespace(),
		Networks:  []ocicni.NetAttachment{},
		ID:        sb.ID(),
		NetNS:     sb.NetNsPath(),
		RuntimeConfig: map[string]ocicni.RuntimeConfig{
			network: {Bandwidth: bwConfig},
		},
	}, nil
}
