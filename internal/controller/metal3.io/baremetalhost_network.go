/*
Copyright 2025 The Metal3 Authors.

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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// BareMetalHost states where the underlying Ironic node allows port updates.
// Maps BMH states to whether port updates are allowed in the corresponding Ironic state.
var allowedPortUpdateStates = map[string]bool{
	string(metal3api.StateRegistering): true, // Ironic: enroll
	string(metal3api.StatePreparing):   true, // Ironic: manageable
	string(metal3api.StateAvailable):   true, // Ironic: available
	string(metal3api.StateInspecting):  true, // Ironic: inspecting
}

// manageSwitchPortConfigs handles validating and managing switch port configurations
// for a given host.
func (r *BareMetalHostReconciler) manageSwitchPortConfigs(ctx context.Context, prov provisioner.Provisioner, info *reconcileInfo) actionResult {
	host := info.host

	info.log.Info("ALEGACY manageSwitchPortConfigs - validate")

	// Validate network interfaces (only after hardware discovery)
	if dirty, err := r.validateNetworkInterfaces(host); err != nil {
		info.log.Error(err, "failed to validate network interfaces")
		return actionError{err}
	} else if dirty {
		info.log.Info("ALEGACY manageSwitchPortConfigs - dirty")
		return actionUpdate{}
	}

	info.log.Info("ALEGACY manageSwitchPortConfigs - apply")

	// Manage network configuration application
	if dirty, err := r.applySwitchPortConfigs(ctx, prov, host); err != nil {
		info.log.Error(err, "failed to manage network configuration")
		return actionError{err}
	} else if dirty {
		return actionUpdate{}
	}

	return actionContinue{}
}

// validateNetworkInterfaces validates that networkInterfaces correspond to actual NICs.
func (r *BareMetalHostReconciler) validateNetworkInterfaces(host *metal3api.BareMetalHost) (bool, error) {
	// Skip validation if no network interfaces specified
	if len(host.Spec.NetworkInterfaces) == 0 {
		r.Log.Info("ALEGACY No network interfaces found")
		return r.clearNetworkInterfaceValidation(host)
	}

	// Skip validation if hardware discovery not complete
	if !r.isHardwareDiscoveryComplete(host) {
		// Clear any existing validation condition since we can't validate yet
		r.Log.Info("ALEGACY No hardware discovery found")
		return r.clearNetworkInterfaceValidation(host)
	}

	// Now we can safely validate since hardware details are available
	r.Log.Info("ALEGACY Do validation")
	return r.performNetworkInterfaceValidation(host)
}

// isHardwareDiscoveryComplete checks if hardware discovery has completed.
func (r *BareMetalHostReconciler) isHardwareDiscoveryComplete(host *metal3api.BareMetalHost) bool {
	// Hardware discovery is complete when:
	// 1. HardwareDetails exists
	// 2. Host has moved past initial registration/inspection phases

	r.Log.Info("ALEGACY IsHardwareDiscoveryComplete", "details", host.Status.HardwareDetails != nil, "state", host.Status.Provisioning.State)

	return host.Status.HardwareDetails != nil
}

// performNetworkInterfaceValidation validates network interfaces against discovered hardware.
func (r *BareMetalHostReconciler) performNetworkInterfaceValidation(host *metal3api.BareMetalHost) (bool, error) {
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
	var validInterfaces []string

	for _, netIf := range host.Spec.NetworkInterfaces {
		key := netIf.GetKey()
		if availableNICs[key] {
			validInterfaces = append(validInterfaces, key)
		} else {
			invalidInterfaces = append(invalidInterfaces, key)
		}
	}

	// Update validation status based on results
	if len(invalidInterfaces) > 0 {
		reason := "InvalidInterfaceNames"
		availableNames := r.getAvailableNICNames(host.Status.HardwareDetails.NIC)
		var message string
		if len(availableNames) == 0 {
			message = fmt.Sprintf("Invalid interface names: %s. No network interfaces discovered on this host.",
				strings.Join(invalidInterfaces, ", "))
		} else {
			message = fmt.Sprintf("Invalid interface names: %s. Available interfaces: %s",
				strings.Join(invalidInterfaces, ", "),
				strings.Join(availableNames, ", "))
		}
		r.Log.Info("ALEGACY invalid interface", "message", message)
		return r.setNetworkInterfaceValidation(host, metav1.ConditionFalse, reason, message)
	}

	r.Log.Info("ALEGACY all interfaces valid")

	reason := "AllInterfacesValid"
	message := "All network interfaces are valid"
	return r.setNetworkInterfaceValidation(host, metav1.ConditionTrue, reason, message)
}

// applySwitchPortConfigs manages applying network configuration to Ironic.
func (r *BareMetalHostReconciler) applySwitchPortConfigs(ctx context.Context, prov provisioner.Provisioner, host *metal3api.BareMetalHost) (bool, error) {
	r.Log.Info("ALEGACY ApplySwitchPortConfigs")
	// Check if network configuration needs to be applied/updated
	needsUpdate, _ := r.switchPortConfigurationNeedsUpdate(host)
	if !needsUpdate {
		return false, nil
	}

	r.Log.Info("ALEGACY ApplySwitchPortConfigs -- needed")

	// Check if Ironic allows port updates in current state
	// If not, we'll retry on next reconcile when state changes
	if !r.ironicAllowsPortUpdates(host) {
		r.Log.Info("pending port updates -- waiting for node state", "state", host.Status.Provisioning.State)
		return false, nil
	}

	r.Log.Info("ALEGACY ApplySwitchPortConfigs -- allowed")

	// Handle network interface removal (empty configurations)
	if len(host.Spec.NetworkInterfaces) == 0 {
		// Apply empty configuration to clear all port configs
		r.Log.Info("ALEGACY ApplySwitchPortConfigs -- remove all")

		if err := prov.SetSwitchPortConfigs(map[string]*provisioner.SwitchPortConfig{}); err != nil {
			return false, fmt.Errorf("failed to clear switch port configurations: %w", err)
		}

		// Clear applied network interfaces
		host.Status.AppliedNetworkInterfaces = nil
		return true, nil
	}

	r.Log.Info("ALEGACY ApplySwitchPortConfigs -- got some")

	// Resolve network attachments to get the configurations
	configs, err := r.resolveSwitchPortConfigs(ctx, host)
	if err != nil {
		return false, fmt.Errorf("failed to resolve switch port configurations: %w", err)
	}

	r.Log.Info("ALEGACY ApplySwitchPortConfigs -- configs", "configs", configs)

	// Apply network configuration to ports (includes cleanup of removed configs)
	if err := prov.SetSwitchPortConfigs(configs); err != nil {
		return false, fmt.Errorf("failed to apply switch port configuration to ports: %w", err)
	}

	// Store the applied network interface configuration
	host.Status.AppliedNetworkInterfaces = make([]metal3api.NetworkInterface, len(host.Spec.NetworkInterfaces))
	copy(host.Status.AppliedNetworkInterfaces, host.Spec.NetworkInterfaces)

	r.Log.Info("ALEGACY ApplySwitchPortConfigs -- applied", "applied", host.Status.AppliedNetworkInterfaces)

	return true, nil
}

// switchPortConfigurationNeedsUpdate checks if switch port configuration needs to be applied.
// Uses drift detection: compares spec.networkInterfaces with status.appliedNetworkInterfaces.
func (r *BareMetalHostReconciler) switchPortConfigurationNeedsUpdate(host *metal3api.BareMetalHost) (bool, string) {
	// No network interfaces specified
	if len(host.Spec.NetworkInterfaces) == 0 {
		// If we previously had configuration applied, we need to clean up
		if len(host.Status.AppliedNetworkInterfaces) > 0 {
			return true, "NetworkInterfacesRemoved"
		}
		return false, ""
	}

	// Network interface validation must pass before we can apply
	if !r.networkInterfaceValidationPassed(host) {
		return false, ""
	}

	// Check if configuration has never been applied
	if host.Status.AppliedNetworkInterfaces == nil {
		return true, "InitialConfiguration"
	}

	// Check if network interface spec has changed since last application
	// This also catches retry cases - if apply failed, AppliedNetworkInterfaces
	// won't be updated, so drift will remain and trigger retry
	if !reflect.DeepEqual(host.Spec.NetworkInterfaces, host.Status.AppliedNetworkInterfaces) {
		return true, "NetworkInterfaceSpecChanged"
	}

	return false, ""
}

// ironicAllowsPortUpdates checks if Ironic allows port updates in the current state.
func (r *BareMetalHostReconciler) ironicAllowsPortUpdates(host *metal3api.BareMetalHost) bool {
	currentState := host.Status.Provisioning.State
	return allowedPortUpdateStates[string(currentState)]
}

// networkInterfaceValidationPassed checks if network interface validation has passed.
func (r *BareMetalHostReconciler) networkInterfaceValidationPassed(host *metal3api.BareMetalHost) bool {
	cond := meta.FindStatusCondition(host.Status.Conditions, metal3api.ConditionNetworkInterfacesValid)
	return cond != nil && cond.Status == metav1.ConditionTrue
}

// setNetworkInterfaceValidation updates the network interface validation condition.
func (r *BareMetalHostReconciler) setNetworkInterfaceValidation(host *metal3api.BareMetalHost, status metav1.ConditionStatus, reason, message string) (bool, error) {
	existing := meta.FindStatusCondition(host.Status.Conditions, metal3api.ConditionNetworkInterfacesValid)
	if existing != nil && existing.Status == status && existing.Reason == reason {
		return false, nil
	}

	meta.SetStatusCondition(&host.Status.Conditions, metav1.Condition{
		Type:    metal3api.ConditionNetworkInterfacesValid,
		Status:  status,
		Reason:  reason,
		Message: message,
	})
	return true, nil
}

// clearNetworkInterfaceValidation removes the network interface validation condition.
func (r *BareMetalHostReconciler) clearNetworkInterfaceValidation(host *metal3api.BareMetalHost) (bool, error) {
	if meta.FindStatusCondition(host.Status.Conditions, metal3api.ConditionNetworkInterfacesValid) != nil {
		meta.RemoveStatusCondition(&host.Status.Conditions, metal3api.ConditionNetworkInterfacesValid)
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

// resolveSwitchPortConfigs resolves network attachments for the given host into a set of
// switch port configurations.
func (r *BareMetalHostReconciler) resolveSwitchPortConfigs(ctx context.Context, host *metal3api.BareMetalHost) (map[string]*provisioner.SwitchPortConfig, error) {
	configs := make(map[string]*provisioner.SwitchPortConfig)

	for _, netIf := range host.Spec.NetworkInterfaces {
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
			// If the attachment is not found, log a warning and skip this interface.
			// This can happen during deprovisioning when attachments are deleted
			// before or during BMH cleanup, or if the attachment was manually removed.
			if apierrors.IsNotFound(err) {
				r.Log.Info("network attachment not found, skipping interface",
					"interface", netIf.GetKey(),
					"attachment", fmt.Sprintf("%s/%s", attachmentNS, netIf.HostNetworkAttachment.Name))
				continue
			}
			// For other errors (permissions, network issues, etc.), fail the reconciliation
			return nil, fmt.Errorf("failed to get network attachment %s/%s: %w",
				attachmentNS, netIf.HostNetworkAttachment.Name, err)
		}

		key := netIf.GetKey()
		configs[key] = &provisioner.SwitchPortConfig{
			Mode:         string(attachment.Spec.Mode),
			NativeVLAN:   attachment.Spec.NativeVLAN,
			AllowedVLANs: attachment.Spec.AllowedVLANs,
			MTU:          attachment.Spec.MTU,
		}
	}

	return configs, nil
}
