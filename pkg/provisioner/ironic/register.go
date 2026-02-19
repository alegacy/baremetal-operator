package ironic

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/baremetal/v1/nodes"
	"github.com/gophercloud/gophercloud/v2/openstack/baremetal/v1/ports"
	metal3api "github.com/metal3-io/baremetal-operator/apis/metal3.io/v1alpha1"
	"github.com/metal3-io/baremetal-operator/pkg/hardwareutils/bmc"
	"github.com/metal3-io/baremetal-operator/pkg/provisioner"
	"github.com/metal3-io/baremetal-operator/pkg/provisioner/ironic/clients"
	"sigs.k8s.io/yaml"
)

const (
	defaultInspectInterface = "agent"
)

func bmcAddressMatches(ironicNode *nodes.Node, driverInfo map[string]any) bool {
	newAddress := make(map[string]any)
	ironicAddress := make(map[string]any)
	reg := regexp.MustCompile("_address$")
	for key, value := range driverInfo {
		if reg.MatchString(key) {
			newAddress[key] = value
			break
		}
	}
	for key, value := range ironicNode.DriverInfo {
		if reg.MatchString(key) {
			ironicAddress[key] = value
			break
		}
	}
	return reflect.DeepEqual(newAddress, ironicAddress)
}

// Register registers the host in the internal database if it does not
// exist, updates the existing host if needed, and tests the connection
// information for the host to verify that the credentials work.
// The credentialsChanged argument tells the provisioner whether the
// current set of credentials it has are different from the credentials
// it has previously been using, without implying that either set of
// credentials is correct.
func (p *ironicProvisioner) Register(data provisioner.ManagementAccessData, credentialsChanged, restartOnFailure bool) (result provisioner.Result, provID string, err error) {
	bmcAccess, err := p.bmcAccess()
	if err != nil {
		result, err = operationFailed(err.Error())
		return result, "", err
	}

	if data.BootMode == metal3api.UEFISecureBoot && !bmcAccess.SupportsSecureBoot() {
		msg := fmt.Sprintf("BMC driver %s does not support secure boot", bmcAccess.Type())
		p.log.Info(msg)
		result, err = operationFailed(msg)
		return result, "", err
	}

	if bmcAccess.RequiresProvisioningNetwork() && p.config.provNetDisabled {
		msg := fmt.Sprintf("BMC driver %s requires a provisioning network", bmcAccess.Type())
		p.log.Info(msg)
		result, err = operationFailed(msg)
		return result, "", err
	}

	// Refuse to manage a node that has Disabled Power off if not supported by ironic,
	// accidentally powering it off would require a arctic expedition to the data center
	if data.DisablePowerOff && !p.availableFeatures.HasDisablePowerOff() {
		msg := "current ironic version does not support DisablePowerOff, refusing to manage node"
		p.log.Info(msg)
		result, err = operationFailed(msg)
		return result, "", err
	}

	var ironicNode *nodes.Node
	updater := clients.UpdateOptsBuilder(p.log)

	p.debugLog.Info("validating management access")

	ironicNode, err = p.findExistingHost(p.bootMACAddress)
	if err != nil {
		var target macAddressConflictError
		if errors.As(err, &target) {
			result, err = operationFailed(target.Error())
		} else {
			result, err = transientError(fmt.Errorf("failed to find existing host: %w", err))
		}
		return result, "", err
	}

	// Some BMC types require a MAC address, so ensure we have one
	// when we need it. If not, place the host in an error state.
	if bmcAccess.NeedsMAC() && p.bootMACAddress == "" {
		msg := fmt.Sprintf("BMC driver %s requires a BootMACAddress value", bmcAccess.Type())
		p.log.Info(msg)
		result, err = operationFailed(msg)
		return result, "", err
	}

	driverInfo := bmcAccess.DriverInfo(p.bmcCreds)
	driverInfo = setExternalURL(p, driverInfo)

	// If we have not found a node yet, we need to create one
	if ironicNode == nil {
		p.log.Info("registering host in ironic")
		var retry bool
		ironicNode, retry, err = p.enrollNode(data, bmcAccess, driverInfo)
		if err != nil {
			result, err = transientError(err)
			return result, "", err
		}
		if retry {
			result, err = retryAfterDelay(provisionRequeueDelay)
			return result, "", err
		}
		// Store the ID so other methods can assume it is set and so
		// we can find the node again later.
		provID = ironicNode.UUID
	} else {
		// FIXME(dhellmann): At this point we have found an existing
		// node in ironic by looking it up. We need to check its
		// settings against what we have in the host, and change them
		// if there are differences.
		provID = ironicNode.UUID

		updater.SetTopLevelOpt("name", ironicNodeName(p.objectMeta), ironicNode.Name)

		// Audit the ports to ensure they match our expected configuration
		err = p.ensurePorts(ironicNode.UUID)
		if err != nil {
			// Port creation failed - return error for retry
			result, err = transientError(fmt.Errorf("failed to ensure ports: %w", err))
			return result, provID, err
		}

		p.log.Info("successfully ensured all ports for existing node",
			"nodeUUID", ironicNode.UUID)

		bmcAddressChanged := !bmcAddressMatches(ironicNode, driverInfo)

		// The actual password is not returned from ironic, so we want to
		// update the whole DriverInfo only if the credentials or BMC address
		// has changed, otherwise we will be writing on every call to this
		// function.
		if credentialsChanged || bmcAddressChanged {
			p.log.Info("Updating driver info because the credentials and/or the BMC address changed")
			updater.SetTopLevelOpt("driver_info", driverInfo, ironicNode.DriverInfo)
		}

		// The updater only updates disable_power_off if it has changed
		updater.SetTopLevelOpt("disable_power_off", data.DisablePowerOff, ironicNode.DisablePowerOff)

		// We don't return here because we also have to set the
		// target provision state to manageable, which happens
		// below.
	}

	// If no PreprovisioningImage builder is enabled we set the Node network_data
	// this enables Ironic to inject the network_data into the ramdisk image
	if !p.config.havePreprovImgBuilder {
		networkDataRaw := data.PreprovisioningNetworkData
		if networkDataRaw != "" {
			var networkData map[string]any
			if yamlErr := yaml.Unmarshal([]byte(networkDataRaw), &networkData); yamlErr != nil {
				p.log.Info("failed to unmarshal networkData from PreprovisioningNetworkData")
				result, err = transientError(fmt.Errorf("invalid preprovisioningNetworkData: %w", yamlErr))
				return result, provID, err
			}
			numUpdates := len(updater.Updates)
			updater.SetTopLevelOpt("network_data", networkData, ironicNode.NetworkData)
			if len(updater.Updates) != numUpdates {
				p.log.Info("adding preprovisioning network_data for node", "node", ironicNode.UUID)
			}
		}
	}

	ironicNode, success, result, err := p.tryUpdateNode(ironicNode, updater)
	if !success {
		return result, provID, err
	}

	p.log.Info("current provision state",
		"lastError", ironicNode.LastError,
		"current", ironicNode.ProvisionState,
		"target", ironicNode.TargetProvisionState,
	)

	// Ensure the node is marked manageable.
	switch nodes.ProvisionState(ironicNode.ProvisionState) {
	case nodes.Enroll:

		// If ironic is reporting an error, stop working on the node.
		if ironicNode.LastError != "" && !(credentialsChanged || restartOnFailure) {
			result, err = operationFailed(ironicNode.LastError)
			return result, provID, err
		}

		if ironicNode.TargetProvisionState == string(nodes.TargetManage) {
			// We have already tried to manage the node and did not
			// get an error, so do nothing and keep trying.
			result, err = operationContinuing(provisionRequeueDelay)
			return result, provID, err
		}

		result, err = p.changeNodeProvisionState(
			ironicNode,
			nodes.ProvisionStateOpts{Target: nodes.TargetManage},
		)
		return result, provID, err

	case nodes.Verifying:
		// If we're still waiting for the state to change in Ironic,
		// return true to indicate that we're dirty and need to be
		// reconciled again.
		result, err = operationContinuing(provisionRequeueDelay)
		return result, provID, err

	case nodes.CleanWait,
		nodes.Cleaning,
		nodes.DeployWait,
		nodes.Deploying,
		nodes.Inspecting:
		// Do not try to update the node if it's in a transient state other than InspectWait - will fail anyway.
		result, err = operationComplete()
		return result, provID, err

	case nodes.Active:
		// The host is already running, maybe it's a controlplane host?
		p.debugLog.Info("have active host", "image_source", ironicNode.InstanceInfo["image_source"])
		fallthrough

	default:
		result, err = p.configureNode(data, ironicNode, bmcAccess)
		return result, provID, err
	}
}

func (p *ironicProvisioner) enrollNode(data provisioner.ManagementAccessData, bmcAccess bmc.AccessDetails, driverInfo map[string]any) (ironicNode *nodes.Node, retry bool, err error) {
	nodeCreateOpts := nodes.CreateOpts{
		Driver:              bmcAccess.Driver(),
		BIOSInterface:       bmcAccess.BIOSInterface(),
		BootInterface:       bmcAccess.BootInterface(),
		Name:                ironicNodeName(p.objectMeta),
		DriverInfo:          driverInfo,
		FirmwareInterface:   bmcAccess.FirmwareInterface(),
		DeployInterface:     p.deployInterface(data),
		InspectInterface:    defaultInspectInterface,
		ManagementInterface: bmcAccess.ManagementInterface(),
		PowerInterface:      bmcAccess.PowerInterface(),
		RAIDInterface:       bmcAccess.RAIDInterface(),
		VendorInterface:     bmcAccess.VendorInterface(),
		DisablePowerOff:     &data.DisablePowerOff,
		Properties: map[string]any{
			"capabilities": buildCapabilitiesValue(nil, data.BootMode),
			"cpu_arch":     data.CPUArchitecture,
		},
	}

	if p.config.enableNetworking {
		nodeCreateOpts.NetworkInterface = p.config.networkInterface
	}

	ironicNode, err = nodes.Create(p.ctx, p.client, nodeCreateOpts).Extract()
	if err == nil {
		p.publisher("Registered", "Registered new host")
	} else if gophercloud.ResponseCodeIs(err, http.StatusConflict) {
		p.log.Info("could not register host in ironic, busy")
		return nil, true, nil
	} else {
		return nil, true, fmt.Errorf("failed to register host in ironic: %w", err)
	}

	// If we know the MAC, create a port. Otherwise we will have
	// to do this after we run the introspection step.
	if p.bootMACAddress != "" {
		err = p.createPXEEnabledNodePort(ironicNode.UUID, p.bootMACAddress,
			p.switchPortConfigs[p.bootMACAddress])
		if err != nil {
			return nil, true, err
		}
	}

	return ironicNode, false, nil
}

// ensurePorts ensures all network interface ports exist in Ironic.
//
// For initial enrollment (before inspection):
//   - Creates only boot MAC port (hardware details not yet available)
//
// For re-registration or post-inspection:
//   - Creates ports for all NICs from stored hardware details
func (p *ironicProvisioner) ensurePorts(nodeUUID string) error {
	hardwareDetails := p.getStoredHardwareDetails()

	// If no hardware details available, fall back to boot MAC only
	// This happens during initial enrollment before inspection
	if hardwareDetails == nil {
		p.log.Info("no stored hardware details available, ensuring boot MAC port only",
			"nodeUUID", nodeUUID)

		if p.bootMACAddress != "" {
			return p.ensurePxePort(nodeUUID)
		}
		return nil
	}

	// TODO(alegacy): should we make this optional?

	// Deduplicate NICs by MAC address to avoid duplicate ports
	uniqueNICs := deduplicateNICsByMAC(hardwareDetails.NIC)

	p.log.Info("ensuring ports for all network interfaces",
		"count", len(uniqueNICs),
		"nodeUUID", nodeUUID)

	// List all existing ports for this node once
	existingPorts, err := p.listNodePorts(nodeUUID)
	if err != nil {
		return fmt.Errorf("failed to list existing ports: %w", err)
	}

	// Build MAC → Port lookup map for fast access
	portsByMAC := make(map[string]ports.Port)
	for _, port := range existingPorts {
		mac := strings.ToLower(port.Address)
		portsByMAC[mac] = port
	}

	// Track failures for error reporting
	var failures []string
	successCount := 0

	// Ensure each unique NIC has a port
	for _, nic := range uniqueNICs {
		if nic.MAC == "" {
			continue // Skip NICs without MAC (should already be filtered)
		}

		isPXEPort := nic.PXE || strings.EqualFold(nic.MAC, p.bootMACAddress)

		// Try to find switch config by interface name first, then by MAC address
		// This supports both NetworkInterface.Name and NetworkInterface.MACAddress keys
		var switchConfig *provisioner.SwitchPortConfig
		if nic.Name != "" {
			switchConfig = p.switchPortConfigs[strings.ToLower(nic.Name)]
		}
		if switchConfig == nil && nic.MAC != "" {
			switchConfig = p.switchPortConfigs[strings.ToLower(nic.MAC)]
		}

		// Check if port already exists
		var existingPort *ports.Port
		if port, exists := portsByMAC[strings.ToLower(nic.MAC)]; exists {
			existingPort = &port
		}

		err := p.ensurePort(nodeUUID, nic, isPXEPort, switchConfig, existingPort)
		if err != nil {
			p.log.Error(err, "failed to ensure port for interface",
				"interface", nic.Name,
				"MAC", nic.MAC)
			failures = append(failures, fmt.Sprintf("%s(%s): %v", nic.Name, nic.MAC, err))
		} else {
			successCount++
		}
	}

	// Report results
	if len(failures) > 0 {
		p.log.Info("port reconciliation completed with failures",
			"successful", successCount,
			"failed", len(failures),
			"total", len(uniqueNICs))
		// Cap the number of failures reported in the error message
		reported := failures
		if len(reported) > 3 {
			reported = reported[:3]
		}
		return fmt.Errorf("failed to ensure %d/%d ports: %s",
			len(failures), len(uniqueNICs), strings.Join(reported, "; "))
	}

	p.log.Info("successfully ensured all ports",
		"count", successCount,
		"nodeUUID", nodeUUID)

	// TODO(alegacy): Remove stale Ironic ports that may no longer correspond
	// to a NIC entry.  Maybe Ironic automatically does this after inspection?

	return nil
}

func (p *ironicProvisioner) ensurePxePort(nodeUUID string) error {
	nodeHasAssignedPort, err := p.nodeHasAssignedPort(nodeUUID)
	if err != nil {
		return err
	}

	if !nodeHasAssignedPort {
		addressIsAllocatedToPort, err := p.isAddressAllocatedToPort(p.bootMACAddress)
		if err != nil {
			return err
		}

		if !addressIsAllocatedToPort {
			err = p.createPXEEnabledNodePort(nodeUUID, p.bootMACAddress,
				p.switchPortConfigs[p.bootMACAddress])
			if err != nil {
				return err
			}
		}
	}

	return nil
}
