package docker

import (
	"fmt"
	"net/netip"
	"strconv"

	"github.com/moby/moby/api/types/network"
	meta "github.com/weaveworks/ignite/pkg/apis/meta/v1alpha1"
)

// portBindingsToDocker takes in portMappings and returns a network.PortMap of
// the port bindings and a network.PortSet of the exposed ports for the Docker
// client. An entry the Engine API cannot express is returned as an error
// rather than silently dropped.
func portBindingsToDocker(portMappings meta.PortMappings) (network.PortMap, network.PortSet, error) {
	bindings, exposed := make(network.PortMap, len(portMappings)), make(network.PortSet, len(portMappings))

	for _, portMapping := range portMappings {
		// The zero netip.Addr marshals to "", matching the previous empty
		// HostIP string when no bind address is given.
		var hostIP netip.Addr
		if portMapping.BindAddress != nil {
			addr, ok := netip.AddrFromSlice(portMapping.BindAddress)
			if !ok {
				// Only 4- and 16-byte addresses exist; anything else would
				// otherwise be sent as "" and silently bind every interface.
				return nil, nil, fmt.Errorf("invalid bind address in port mapping %q: %d-byte IP", portMapping.String(), len(portMapping.BindAddress))
			}
			hostIP = addr.Unmap()
		}

		protocol := portMapping.Protocol
		if len(protocol) == 0 {
			// Docker uses TCP by default
			protocol = meta.ProtocolTCP
		}

		port, err := network.ParsePort(fmt.Sprintf("%d/%s", portMapping.VMPort, protocol.String()))
		if err != nil {
			return nil, nil, fmt.Errorf("invalid port mapping %q: %w", portMapping.String(), err)
		}
		exposed[port] = struct{}{}
		bindings[port] = []network.PortBinding{
			{
				HostIP:   hostIP,
				HostPort: strconv.FormatUint(portMapping.HostPort, 10),
			},
		}
	}

	return bindings, exposed, nil
}
