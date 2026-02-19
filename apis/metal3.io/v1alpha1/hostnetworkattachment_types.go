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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NOTE: json tags are required.  Any new fields you add must have
// json tags for the fields to be serialized.

// NOTE: Update docs/api.md when changing these data structure.

// HostNetworkAttachment CRD Design
//
// Purpose:
// HostNetworkAttachment provides declarative configuration for bare metal host network
// switch ports. It maps to Ironic's standalone networking feature, allowing administrators
// to configure VLAN and MTU settings on network interfaces via Ironic's networking drivers.
//
// Relationship to Ironic:
// When applied to a BareMetalHost's NetworkInterface, the attachment configuration is
// translated to the Ironic port's extra.switchport field. Ironic networking drivers
// (such as those for various network switches) read this configuration and apply it
// to the actual switch ports.
//
// Use Cases:
//  1. VLAN Configuration: Assign hosts to specific VLANs for network segmentation
//  2. Trunk Configuration: Enable multiple VLANs on a single interface
//  3. MTU Configuration: Set jumbo frames for high-performance workloads
//
// Immutability:
// The spec fields are immutable while any BareMetalHost references the attachment.
// This prevents accidental configuration changes that could disrupt running hosts.
// To modify an in-use attachment:
//  1. Remove references from all BMH resources
//  2. Update the attachment
//  3. Re-add references to apply new configuration
// Alternatively, create a new attachment with desired configuration.
//
// Example Usage:
//
//	# Create access mode attachment (single VLAN)
//	apiVersion: metal3.io/v1alpha1
//	kind: HostNetworkAttachment
//	metadata:
//	  name: provisioning-network
//	  namespace: metal3
//	spec:
//	  mode: access
//	  nativeVLAN: 100
//	  mtu: 9000
//
//	# Create trunk mode attachment (multiple VLANs)
//	apiVersion: metal3.io/v1alpha1
//	kind: HostNetworkAttachment
//	metadata:
//	  name: tenant-networks
//	  namespace: metal3
//	spec:
//	  mode: trunk
//	  nativeVLAN: 1
//	  allowedVLANs: [100, 200, 300]
//	  mtu: 1500
//
//	# Reference from BareMetalHost
//	apiVersion: metal3.io/v1alpha1
//	kind: BareMetalHost
//	metadata:
//	  name: worker-0
//	spec:
//	  networkInterfaces:
//	  - name: eth0  # Or macAddress: "aa:bb:cc:dd:ee:ff"
//	    hostNetworkAttachment:
//	      name: provisioning-network
//

// HostNetworkAttachmentRef references a HostNetworkAttachment for interface configuration.
type HostNetworkAttachmentRef struct {
	// Name of the HostNetworkAttachment resource
	Name string `json:"name,omitempty"`

	// Namespace of the HostNetworkAttachment (defaults to BMH namespace)
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// SwitchportMode defines the switchport mode for network interfaces.
// +kubebuilder:validation:Enum=access;trunk;hybrid
type SwitchportMode string

const (
	// SwitchportModeAccess sets the interface to access mode (single VLAN)
	SwitchportModeAccess SwitchportMode = "access"
	// SwitchportModeTrunk sets the interface to trunk mode (multiple VLANs)
	SwitchportModeTrunk SwitchportMode = "trunk"
	// SwitchportModeHybrid sets the interface to hybrid mode (access + trunk)
	SwitchportModeHybrid SwitchportMode = "hybrid"
)

// HostNetworkAttachmentSpec defines the desired switchport configuration.
type HostNetworkAttachmentSpec struct {
	// Mode specifies the network attachment mode.
	// +kubebuilder:validation:Enum=access;hybrid;trunk
	// +kubebuilder:required
	// +kubebuilder:default=access
	Mode SwitchportMode `json:"mode"`

	// NativeVLAN specifies the native VLAN ID for the network attachment.
	// This is the untagged VLAN used on the interface.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=4094
	NativeVLAN int `json:"nativeVLAN"`

	// AllowedVLANs specifies a list of VLAN IDs that are allowed on this network attachment.
	// This is typically used in trunk or hybrid mode to specify which tagged VLANs can be carried on the interface.
	// +optional
	AllowedVLANs []int `json:"allowedVLANs,omitempty"`

	// MTU specifies the Maximum Transmission Unit size for the network attachment.
	// If not specified, the default MTU for the underlying network will be used.
	// +optional
	MTU *int `json:"mtu,omitempty"`
}

// HostNetworkAttachment defines switchport configuration for BMH network interfaces.
// Spec fields are mutable when no BMH references the attachment, immutable when in use.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Mode",type="string",JSONPath=".spec.mode",description="Switchport mode"
// +kubebuilder:printcolumn:name="Native VLAN",type="integer",JSONPath=".spec.nativeVLAN",description="Native VLAN ID"
// +kubebuilder:printcolumn:name="MTU",type="integer",JSONPath=".spec.mtu",description="Maximum Transmission Unit"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Time duration since creation"
type HostNetworkAttachment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec HostNetworkAttachmentSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// HostNetworkAttachmentList contains a list of HostNetworkAttachment.
type HostNetworkAttachmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HostNetworkAttachment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HostNetworkAttachment{}, &HostNetworkAttachmentList{})
}
