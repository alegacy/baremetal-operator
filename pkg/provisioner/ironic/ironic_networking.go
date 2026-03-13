package ironic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/gophercloud/gophercloud/v2/openstack/baremetal/v1/ports"
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
func (p *ironicProvisioner) updatePort(ctx context.Context, existingPort ports.Port, portConfig *provisioner.PortConfig) error {
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

// EnsurePorts ensures all network ports in Ironic have the correct switch
// port configurations applied. It uses the switch port configs stored in
// the provisioner (from HostData) and matches them to ports by interface
// name or MAC address.
func (p *ironicProvisioner) EnsurePorts(ctx context.Context) error {
	if !p.config.enableNetworking {
		return nil
	}

	if p.nodeID == "" {
		return errors.New("cannot ensure ports: node not registered")
	}

	p.log.Info("ensuring port configs", "count", len(p.portConfigs))

	// List existing ports for this node
	existingPorts, err := p.listNodePorts(ctx, p.nodeID)
	if err != nil {
		return fmt.Errorf("failed to list ports: %w", err)
	}

	for _, port := range existingPorts {
		mac := strings.ToLower(port.Address)
		config := p.portConfigs[mac]
		if err := p.updatePort(ctx, port, config); err != nil {
			return err
		}
	}

	return nil
}
