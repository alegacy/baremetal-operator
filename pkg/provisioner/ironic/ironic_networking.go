package ironic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/gophercloud/gophercloud/v2/openstack/baremetal/v1/ports"
	metal3api "github.com/metal3-io/baremetal-operator/apis/metal3.io/v1alpha1"
	"github.com/metal3-io/baremetal-operator/pkg/provisioner"
)

// buildLocalLinkFromConfig builds a local_link_connection map from manual
// switch port identity in the SwitchPortConfig. This is used when operators
// provide switch port data directly instead of relying on LLDP inspection.
func buildLocalLinkFromConfig(config *provisioner.LocalLinkConnection) map[string]interface{} {
	llc := make(map[string]interface{})
	if config.SwitchID != "" {
		llc["switch_id"] = config.SwitchID
	}
	if config.PortID != "" {
		llc["port_id"] = config.PortID
	}
	return llc
}

// buildLocalLinkFromNIC creates a local_link_connection map from stored LLDP data.
func buildLocalLinkFromNIC(nic metal3api.NIC) map[string]interface{} {
	if nic.LLDP == nil {
		return nil
	}

	connection := make(map[string]interface{})

	if nic.LLDP.SwitchID != "" {
		connection["switch_id"] = nic.LLDP.SwitchID
	}
	if nic.LLDP.PortID != "" {
		connection["port_id"] = nic.LLDP.PortID
	}
	if nic.LLDP.SwitchSystemName != "" {
		connection["switch_info"] = nic.LLDP.SwitchSystemName
	}

	if len(connection) == 0 {
		return nil
	}

	return connection
}

// parseSwitchPortConfig converts a switchport config from Ironic (map[string]any)
// into a SwitchPortConfig struct via JSON round-trip to handle type coercion.
func parseSwitchPortConfig(raw any) *provisioner.SwitchPortConfig {
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	config := &provisioner.SwitchPortConfig{}
	if err := json.Unmarshal(data, config); err != nil {
		return nil
	}
	return config
}

// switchPortConfigsEqual compares a switchport config from Ironic (map[string]any)
// with a new SwitchPortConfig struct, handling JSON type conversions.
func switchPortConfigsEqual(existing any, desired *provisioner.SwitchPortConfig) bool {
	parsed := parseSwitchPortConfig(existing)
	if parsed == nil {
		return false
	}

	// Normalize nil vs empty slice for comparison
	if len(parsed.AllowedVLANs) == 0 {
		parsed.AllowedVLANs = nil
	}
	if len(desired.AllowedVLANs) == 0 {
		desired.AllowedVLANs = nil
	}

	return reflect.DeepEqual(parsed, desired)
}

// updatePort updates port with local_link_connection and switchport data.
func (p *ironicProvisioner) updatePort(ctx context.Context, existingPort ports.Port, nic metal3api.NIC, portConfig *provisioner.PortConfig) error {
	var updateOpts ports.UpdateOpts

	// Add switch port config if available; otherwise remove
	if portConfig != nil {
		// Compare new config with existing to avoid unnecessary updates
		if !switchPortConfigsEqual(existingPort.Extra["switchport"], &portConfig.SwitchPortConfig) {
			op := ports.AddOp
			if _, exists := existingPort.Extra["switchport"]; exists {
				op = ports.ReplaceOp
			}
			updateOpts = append(updateOpts, ports.UpdateOperation{
				Op:    op,
				Path:  "/extra/switchport",
				Value: portConfig.SwitchPortConfig,
			})
		}

		if portConfig.LocalLinkConnection != nil {
			llc := buildLocalLinkFromConfig(portConfig.LocalLinkConnection)
			if !reflect.DeepEqual(existingPort.LocalLinkConnection, llc) {
				updateOpts = append(updateOpts, ports.UpdateOperation{
					Op:    ports.ReplaceOp,
					Path:  "/local_link_connection",
					Value: llc,
				})
			}
		} else if len(existingPort.LocalLinkConnection) == 0 {
			// No manual override — fall back to LLDP data from inspection
			if llc := buildLocalLinkFromNIC(nic); llc != nil {
				updateOpts = append(updateOpts, ports.UpdateOperation{
					Op:    ports.ReplaceOp,
					Path:  "/local_link_connection",
					Value: llc,
				})
			}
		}
	} else if existingPort.Extra != nil && existingPort.Extra["switchport"] != nil {
		updateOpts = append(updateOpts, ports.UpdateOperation{
			Op:   ports.RemoveOp,
			Path: "/extra/switchport",
		})

		// NOTE(alegacy): Don't remove any local_link_connection data as it
		// may have been provided by LLDP.
	}

	if len(updateOpts) > 0 {
		_, err := ports.Update(ctx, p.client, existingPort.UUID, updateOpts).Extract()
		if err != nil {
			return fmt.Errorf("failed to update local_link_connection for port %s: %w", existingPort.UUID, err)
		}
	}

	return nil
}

// createPort creates new port with LLDP data (only called when port doesn't exist).
func (p *ironicProvisioner) createPort(ctx context.Context, nodeUUID string, nic metal3api.NIC, pxeEnabled bool, portConfig *provisioner.PortConfig) error {
	createOpts := ports.CreateOpts{
		NodeUUID:   nodeUUID,
		Address:    nic.MAC,
		PXEEnabled: &pxeEnabled,
	}

	// Set switch port configuration
	if portConfig != nil {
		createOpts.Extra = map[string]interface{}{
			"switchport": portConfig.SwitchPortConfig,
		}
		p.log.Info("setting extra.switchport on new port",
			"interface", nic.Name,
			"switchport", portConfig.SwitchPortConfig)
	}

	if portConfig != nil && portConfig.LocalLinkConnection != nil {
		// If a manual override of LLC was provided then use it.
		createOpts.LocalLinkConnection = buildLocalLinkFromConfig(portConfig.LocalLinkConnection)
	} else if llc := buildLocalLinkFromNIC(nic); llc != nil {
		// Otherwise, fallback to the inspection data from LLDP if available
		createOpts.LocalLinkConnection = llc
	}

	_, err := ports.Create(ctx, p.client, createOpts).Extract()
	if err != nil {
		return fmt.Errorf("failed to create ironic port for node %s, interface %s, MAC: %s: %w",
			nodeUUID, nic.Name, nic.MAC, err)
	}

	return nil
}

// ensurePort ensures a port exists for the given NIC with proper LLDP and switch configuration.
func (p *ironicProvisioner) ensurePort(
	ctx context.Context,
	nodeUUID string,
	nic metal3api.NIC,
	pxeEnabled bool,
	portConfig *provisioner.PortConfig,
	existingPort *ports.Port,
) error {
	if existingPort != nil {
		return p.updatePort(ctx, *existingPort, nic, portConfig)
	}

	// Port doesn't exist - create new port with LLDP data
	p.log.Info("creating new port",
		"MAC", nic.MAC,
		"interface", nic.Name,
		"pxeEnabled", pxeEnabled,
		"node", nodeUUID)

	return p.createPort(ctx, nodeUUID, nic, pxeEnabled, portConfig)
}

// EnsurePorts ensures all network ports exist in Ironic.
// This is called:
//   - After inspection completes (to create ports for all discovered NICs)
//   - During re-registration (to recreate ports after Ironic database loss)
func (p *ironicProvisioner) EnsurePorts(ctx context.Context) error {
	if !p.config.enableNetworking {
		// If the network feature isn't enabled then maintain the existing
		// behaviour where only the PXE enabled port is created at registration.
		return nil
	}

	if p.nodeID == "" {
		return errors.New("cannot ensure ports: node not registered")
	}

	return p.ensurePorts(ctx, p.nodeID)
}
