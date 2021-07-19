package hostport

import (
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"
	iptablesproxy "k8s.io/kubernetes/pkg/proxy/iptables"

	utiliptables "k8s.io/kubernetes/pkg/util/iptables"
	utilexec "k8s.io/utils/exec"
	utilnet "k8s.io/utils/net"
)

type metaHostportManager struct {
	ipv4HostportManager HostPortManager
	ipv6HostportManager HostPortManager
}

// NewMetaHostportManager creates a new HostPortManager
func NewMetaHostportManager() HostPortManager {
	exec := utilexec.New()
	// Create IPv4 handler
	iptInterface := utiliptables.New(exec, utiliptables.ProtocolIPv4)
	if _, err := iptInterface.EnsureChain(utiliptables.TableNAT, iptablesproxy.KubeMarkMasqChain); err != nil {
		logrus.Warnf("Unable to ensure iptables chain: %v", err)
	}
	hostportManagerv4 := NewHostportManager(iptInterface)
	// Create IPv6 handler
	ip6tInterface := utiliptables.New(exec, utiliptables.ProtocolIPv6)
	if _, err := ip6tInterface.EnsureChain(utiliptables.TableNAT, iptablesproxy.KubeMarkMasqChain); err != nil {
		logrus.Warnf("Unable to ensure ip6tables chain: %v", err)
	}
	hostportManagerv6 := NewHostportManager(ip6tInterface)

	h := &metaHostportManager{
		ipv4HostportManager: hostportManagerv4,
		ipv6HostportManager: hostportManagerv6,
	}
	return h
}

func (mh *metaHostportManager) Add(id string, podPortMapping *PodPortMapping, natInterfaceName string) error {
	if utilnet.IsIPv6(podPortMapping.IP) {
		return mh.ipv6HostportManager.Add(id, podPortMapping, natInterfaceName)
	}

	return mh.ipv4HostportManager.Add(id, podPortMapping, natInterfaceName)
}

func (mh *metaHostportManager) Remove(id string, podPortMapping *PodPortMapping) error {
	var errstrings []string
	// Remove may not have the IP information, so we try to clean us much as possible
	// and warn about the possible errors
	err := mh.ipv4HostportManager.Remove(id, podPortMapping)
	if err != nil {
		errstrings = append(errstrings, err.Error())
	}
	err = mh.ipv6HostportManager.Remove(id, podPortMapping)
	if err != nil {
		errstrings = append(errstrings, err.Error())
	}
	if len(errstrings) > 0 {
		return fmt.Errorf(strings.Join(errstrings, "\n"))
	}
	return nil
}
