/*

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"

	metal3api "github.com/metal3-io/baremetal-operator/apis/metal3.io/v1alpha1"
	"github.com/metal3-io/baremetal-operator/pkg/provisioner"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// managePortConfigs handles validating and applying switch port configurations
// for a given host. resolvedConfigs contains the switch port configurations resolved
// from HostNetworkAttachment resources at the start of reconciliation.
func (r *BareMetalHostReconciler) managePortConfigs(ctx context.Context, prov provisioner.Provisioner, info *reconcileInfo) actionResult {
	host := info.host
	dirty := false

	// Validate network interfaces
	if validationDirty, err := r.validateNetworkInterfaces(ctx, host); err != nil {
		info.log.Error(err, "failed to validate network interfaces")
		return actionError{err}
	} else if validationDirty {
		dirty = true
	}

	// Apply network configuration
	if configDirty, err := r.applyPortConfigs(ctx, prov, host, info); err != nil {
		info.log.Error(err, "failed to apply network configuration")
		return actionError{err}
	} else if configDirty {
		dirty = true
	}

	if dirty {
		return actionUpdate{}
	}
	return actionContinue{}
}

// validateNetworkInterfaces validates that networkInterfaces correspond to actual NICs
// and that referenced HostNetworkAttachments exist.
func (r *BareMetalHostReconciler) validateNetworkInterfaces(ctx context.Context, host *metal3api.BareMetalHost) (bool, error) {
	// Skip validation if no network interfaces specified
	if len(host.Spec.NetworkInterfaces) == 0 {
		return r.clearNetworkInterfaceValidation(host)
	}

	// Skip validation if hardware discovery not complete
	if !r.isHardwareDiscoveryComplete(host) {
		return r.setNetworkInterfaceValidation(host, metav1.ConditionFalse,
			"HardwareDiscoveryIncomplete",
			"Waiting for hardware discovery to complete before validating network interfaces")
	}

	// Now we can safely validate since hardware details are available
	return r.performNetworkInterfaceValidation(ctx, host)
}

// isHardwareDiscoveryComplete checks if hardware discovery has completed.
func (r *BareMetalHostReconciler) isHardwareDiscoveryComplete(host *metal3api.BareMetalHost) bool {
	return host.Status.HardwareDetails != nil
}

// performNetworkInterfaceValidation validates network interfaces against discovered hardware
// and checks that referenced HostNetworkAttachments exist.
func (r *BareMetalHostReconciler) performNetworkInterfaceValidation(ctx context.Context, host *metal3api.BareMetalHost) (bool, error) {
	// Build map of available NIC names from hardware details
	availableNICs := make(map[string]bool)
	for _, nic := range host.Status.HardwareDetails.NIC {
		if nic.Name != "" {
			availableNICs[nic.Name] = true
		}
		if nic.MAC != "" {
			availableNICs[nic.MAC] = true
		}
	}

	// Validate each specified network interface
	var invalidInterfaces []string
	var missingAttachments []string

	for _, netIf := range host.Spec.NetworkInterfaces {
		key := netIf.GetKey()
		if !availableNICs[key] {
			invalidInterfaces = append(invalidInterfaces, key)
		}

		// Check that referenced HostNetworkAttachment exists
		if netIf.HostNetworkAttachment.Name != "" {
			attachment := &metal3api.HostNetworkAttachment{}
			attachmentNS := netIf.HostNetworkAttachment.Namespace
			if attachmentNS == "" {
				attachmentNS = host.Namespace
			}
			err := r.Get(ctx, types.NamespacedName{
				Name:      netIf.HostNetworkAttachment.Name,
				Namespace: attachmentNS,
			}, attachment)
			if err != nil {
				if k8serrors.IsNotFound(err) {
					missingAttachments = append(missingAttachments,
						fmt.Sprintf("%s/%s (interface %s)", attachmentNS, netIf.HostNetworkAttachment.Name, key))
				} else {
					return false, fmt.Errorf("failed to check attachment %s/%s: %w",
						attachmentNS, netIf.HostNetworkAttachment.Name, err)
				}
			}
		}
	}

	// Update validation status based on results
	if len(invalidInterfaces) > 0 {
		reason := "InvalidInterfaceNames"
		availableNames := r.getAvailableNICNames(host.Status.HardwareDetails.NIC)
		var message string
		if len(availableNames) == 0 {
			message = fmt.Sprintf("Invalid interface names: %s. No such network interfaces.",
				strings.Join(invalidInterfaces, ", "))
		} else {
			message = fmt.Sprintf("Invalid interface names: %s. Available interfaces: %s",
				strings.Join(invalidInterfaces, ", "),
				strings.Join(availableNames, ", "))
		}
		return r.setNetworkInterfaceValidation(host, metav1.ConditionFalse, reason, message)
	}

	if len(missingAttachments) > 0 {
		reason := "AttachmentNotFound"
		message := "HostNetworkAttachment not found: " +
			strings.Join(missingAttachments, ", ")
		return r.setNetworkInterfaceValidation(host, metav1.ConditionFalse, reason, message)
	}

	reason := "AllInterfacesValid"
	message := "All network interfaces and attachments are valid"
	return r.setNetworkInterfaceValidation(host, metav1.ConditionTrue, reason, message)
}

// applyPortConfigs manages applying network configuration to Ironic.
// resolvedConfigs contains the switch port configurations resolved from
// HostNetworkAttachment resources.
func (r *BareMetalHostReconciler) applyPortConfigs(ctx context.Context, prov provisioner.Provisioner, host *metal3api.BareMetalHost, info *reconcileInfo) (bool, error) {
	// Check if network configuration needs to be applied/updated
	needsUpdate := r.portConfigsNeedUpdate(host, info)
	if !needsUpdate {
		return false, nil
	}

	// Apply switch port configs via provisioner. The provisioner already has
	// the resolved configs from HostData (set at the top of Reconcile), so no
	// need to re-resolve here.
	if err := prov.EnsurePorts(ctx); err != nil {
		return false, fmt.Errorf("failed to apply switch port configuration to ports: %w", err)
	}

	// Handle network interface removal (empty configurations)
	if len(host.Spec.NetworkInterfaces) == 0 {
		host.Status.AppliedPortConfigs = nil
		return true, nil
	}

	// Store the resolved configs that were actually applied, keyed by
	// interface name. This records concrete values (VLAN/MTU/mode) rather
	// than HNA references, so drift detection catches HNA spec changes
	// and deletions.
	host.Status.AppliedPortConfigs = buildAppliedPortConfigs(host, info)

	return true, nil
}

// buildAppliedPortConfigs builds the applied status from the resolved
// configs map and the host's network interfaces.
func buildAppliedPortConfigs(host *metal3api.BareMetalHost, info *reconcileInfo) []metal3api.AppliedPortConfig {
	macToName := buildNICMacToNameMap(host)
	applied := make([]metal3api.AppliedPortConfig, 0, len(info.portConfigs))

	for mac, config := range info.portConfigs {
		appliedPortConfig := metal3api.AppliedPortConfig{
			Name: macToName[mac],
			SwitchPortConfig: metal3api.SwitchPortConfig{
				Mode:         metal3api.SwitchPortMode(config.SwitchPortConfig.Mode),
				NativeVLAN:   config.SwitchPortConfig.NativeVLAN,
				AllowedVLANs: config.SwitchPortConfig.AllowedVLANs,
				MTU:          config.SwitchPortConfig.MTU,
			},
		}
		if config.LocalLinkConnection != nil {
			llc := metal3api.LocalLinkConnection{
				SwitchID: config.LocalLinkConnection.SwitchID,
				PortID:   config.LocalLinkConnection.PortID,
			}
			appliedPortConfig.LocalLinkConnection = &llc
		}
		applied = append(applied, appliedPortConfig)
	}

	return applied
}

// portConfigsNeedUpdate checks if switch port configuration needs
// to be applied. Compares the currently resolved configs against what was last
// applied (stored in status.AppliedPortConfigs).
func (r *BareMetalHostReconciler) portConfigsNeedUpdate(host *metal3api.BareMetalHost, info *reconcileInfo) bool {
	// No network interfaces specified
	if len(host.Spec.NetworkInterfaces) == 0 {
		// If we previously had configuration applied, we need to clean up
		return len(host.Status.AppliedPortConfigs) > 0
	}

	// Skip if network interface validation has explicitly failed.
	// When the condition doesn't exist yet (NI just added), we allow
	// through so that handleAvailable can trigger preparing.
	cond := meta.FindStatusCondition(host.Status.Conditions, metal3api.NetworkInterfacesValidCondition)
	if cond != nil && cond.Status == metav1.ConditionFalse {
		return false
	}

	// Build what the applied status would look like with current configs
	desired := buildAppliedPortConfigs(host, info)

	// Compare with what's currently recorded as applied
	return !reflect.DeepEqual(desired, host.Status.AppliedPortConfigs)
}

// setNetworkInterfaceValidation updates the network interface validation condition.
func (r *BareMetalHostReconciler) setNetworkInterfaceValidation(host *metal3api.BareMetalHost, status metav1.ConditionStatus, reason, message string) (bool, error) {
	existing := meta.FindStatusCondition(host.Status.Conditions, metal3api.NetworkInterfacesValidCondition)
	if existing != nil && existing.Status == status && existing.Reason == reason {
		return false, nil
	}

	meta.SetStatusCondition(&host.Status.Conditions, metav1.Condition{
		Type:    metal3api.NetworkInterfacesValidCondition,
		Status:  status,
		Reason:  reason,
		Message: message,
	})
	return true, nil
}

// clearNetworkInterfaceValidation removes the network interface validation condition.
func (r *BareMetalHostReconciler) clearNetworkInterfaceValidation(host *metal3api.BareMetalHost) (bool, error) {
	if meta.FindStatusCondition(host.Status.Conditions, metal3api.NetworkInterfacesValidCondition) != nil {
		meta.RemoveStatusCondition(&host.Status.Conditions, metal3api.NetworkInterfacesValidCondition)
		return true, nil
	}
	return false, nil
}

// getAvailableNICNames returns a sorted list of available NIC names.
func (r *BareMetalHostReconciler) getAvailableNICNames(nics []metal3api.NIC) []string {
	names := make([]string, 0)
	for _, nic := range nics {
		if nic.Name != "" {
			names = append(names, nic.Name)
		}
	}
	sort.Strings(names)
	return names
}

// buildNICNameToMACMap builds a map from NIC name to MAC address using
// the host's discovered hardware details.
func buildNICNameToMACMap(host *metal3api.BareMetalHost) map[string]string {
	nameToMAC := make(map[string]string)
	if host.Status.HardwareDetails == nil {
		return nameToMAC
	}
	for _, nic := range host.Status.HardwareDetails.NIC {
		if nic.Name != "" && nic.MAC != "" {
			nameToMAC[nic.Name] = strings.ToLower(nic.MAC)
		}
	}
	return nameToMAC
}

// buildNICMacToNameMap builds a map from NIC mac address to name using
// the host's discovered hardware details.
func buildNICMacToNameMap(host *metal3api.BareMetalHost) map[string]string {
	macToName := make(map[string]string)
	if host.Status.HardwareDetails == nil {
		return macToName
	}
	for _, nic := range host.Status.HardwareDetails.NIC {
		if nic.Name != "" && nic.MAC != "" {
			macToName[strings.ToLower(nic.MAC)] = nic.Name
		}
	}
	return macToName
}

func (r *BareMetalHostReconciler) resolveSwitchPortConfig(ctx context.Context, namespace string, netIf *metal3api.NetworkInterface, config *provisioner.SwitchPortConfig) (bool, error) {
	if netIf.HostNetworkAttachment.Name == "" {
		// This should get caught earlier in validation so should never get here
		return true, nil
	}

	attachment := &metal3api.HostNetworkAttachment{}
	attachmentNS := netIf.HostNetworkAttachment.Namespace
	if attachmentNS == "" {
		attachmentNS = namespace
	}

	err := r.Get(ctx, types.NamespacedName{
		Name:      netIf.HostNetworkAttachment.Name,
		Namespace: attachmentNS,
	}, attachment)

	if err != nil {
		if k8serrors.IsNotFound(err) {
			r.Log.Info("network attachment not found, skipping interface",
				"interface", netIf.GetKey(),
				"attachment", fmt.Sprintf("%s/%s", attachmentNS, netIf.HostNetworkAttachment.Name))
			return true, nil
		}

		return false, fmt.Errorf("failed to get network attachment %s/%s: %w",
			attachmentNS, netIf.HostNetworkAttachment.Name, err)
	}

	config.Mode = string(attachment.Spec.Mode)
	config.NativeVLAN = attachment.Spec.NativeVLAN
	config.AllowedVLANs = attachment.Spec.AllowedVLANs
	config.MTU = attachment.Spec.MTU

	return false, nil
}

func (r *BareMetalHostReconciler) resolveLocalLinkConnectionConfig(netIf *metal3api.NetworkInterface, config *provisioner.PortConfig) {
	if netIf.SwitchPort != nil {
		llc := &provisioner.LocalLinkConnection{}
		llc.SwitchID = netIf.SwitchPort.SwitchID
		llc.PortID = netIf.SwitchPort.PortID
		config.LocalLinkConnection = llc
	}
}

// resolvePortConfigs resolves network attachments for the given host into a set
// of port configurations keyed by MAC address.
func (r *BareMetalHostReconciler) resolvePortConfigs(ctx context.Context, host *metal3api.BareMetalHost) (map[string]*provisioner.PortConfig, error) {
	configs := make(map[string]*provisioner.PortConfig)
	nameToMAC := buildNICNameToMACMap(host)

	for _, netIf := range host.Spec.NetworkInterfaces {
		// Resolve MAC address key for this interface
		var macKey string
		if netIf.MACAddress != "" {
			macKey = strings.ToLower(netIf.MACAddress)
		} else if mac, ok := nameToMAC[netIf.Name]; ok {
			macKey = mac
		} else {
			r.Log.Info("cannot resolve interface name to MAC address, skipping",
				"interface", netIf.Name)
			continue
		}

		config := &provisioner.PortConfig{}
		if skip, err := r.resolveSwitchPortConfig(ctx, host.Namespace, &netIf, &config.SwitchPortConfig); err != nil {
			return nil, err
		} else if skip {
			continue
		}

		// Populate manual switch port identity if provided.
		// This overrides LLDP-discovered data in the provisioner.
		r.resolveLocalLinkConnectionConfig(&netIf, config)

		configs[macKey] = config
	}

	return configs, nil
}
