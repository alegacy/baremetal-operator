package controllers

import (
	"context"
	"testing"

	metal3api "github.com/metal3-io/baremetal-operator/apis/metal3.io/v1alpha1"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestValidateNetworkInterfaces(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = metal3api.AddToScheme(scheme)

	testCases := []struct {
		name                   string
		host                   *metal3api.BareMetalHost
		expectedDirty          bool
		expectedValidationPass bool
		expectedReason         string
	}{
		{
			name: "no-network-interfaces-specified",
			host: &metal3api.BareMetalHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-host",
					Namespace: "test-ns",
				},
				Spec: metal3api.BareMetalHostSpec{},
			},
			expectedDirty:          false,
			expectedValidationPass: false,
		},
		{
			name: "hardware-discovery-not-complete",
			host: &metal3api.BareMetalHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-host",
					Namespace: "test-ns",
				},
				Spec: metal3api.BareMetalHostSpec{
					NetworkInterfaces: []metal3api.NetworkInterface{
						{Name: "eth0"},
					},
				},
				Status: metal3api.BareMetalHostStatus{
					Provisioning: metal3api.ProvisionStatus{
						State: metal3api.StateRegistering,
					},
				},
			},
			expectedDirty:          false,
			expectedValidationPass: false,
		},
		{
			name: "all-interfaces-valid-by-name",
			host: &metal3api.BareMetalHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-host",
					Namespace: "test-ns",
				},
				Spec: metal3api.BareMetalHostSpec{
					NetworkInterfaces: []metal3api.NetworkInterface{
						{Name: "eth0"},
						{Name: "eth1"},
					},
				},
				Status: metal3api.BareMetalHostStatus{
					Provisioning: metal3api.ProvisionStatus{
						State: metal3api.StateAvailable,
					},
					HardwareDetails: &metal3api.HardwareDetails{
						NIC: []metal3api.NIC{
							{Name: "eth0", MAC: "00:11:22:33:44:55"},
							{Name: "eth1", MAC: "00:11:22:33:44:66"},
						},
					},
				},
			},
			expectedDirty:          true,
			expectedValidationPass: true,
			expectedReason:         "AllInterfacesValid",
		},
		{
			name: "all-interfaces-valid-by-mac",
			host: &metal3api.BareMetalHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-host",
					Namespace: "test-ns",
				},
				Spec: metal3api.BareMetalHostSpec{
					NetworkInterfaces: []metal3api.NetworkInterface{
						{MACAddress: "00:11:22:33:44:55"},
					},
				},
				Status: metal3api.BareMetalHostStatus{
					Provisioning: metal3api.ProvisionStatus{
						State: metal3api.StateAvailable,
					},
					HardwareDetails: &metal3api.HardwareDetails{
						NIC: []metal3api.NIC{
							{Name: "eth0", MAC: "00:11:22:33:44:55"},
						},
					},
				},
			},
			expectedDirty:          true,
			expectedValidationPass: true,
			expectedReason:         "AllInterfacesValid",
		},
		{
			name: "invalid-interface-name",
			host: &metal3api.BareMetalHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-host",
					Namespace: "test-ns",
				},
				Spec: metal3api.BareMetalHostSpec{
					NetworkInterfaces: []metal3api.NetworkInterface{
						{Name: "invalid-interface"},
					},
				},
				Status: metal3api.BareMetalHostStatus{
					Provisioning: metal3api.ProvisionStatus{
						State: metal3api.StateAvailable,
					},
					HardwareDetails: &metal3api.HardwareDetails{
						NIC: []metal3api.NIC{
							{Name: "eth0", MAC: "00:11:22:33:44:55"},
						},
					},
				},
			},
			expectedDirty:          true,
			expectedValidationPass: false,
			expectedReason:         "InvalidInterfaceNames",
		},
		{
			name: "mixed-valid-and-invalid",
			host: &metal3api.BareMetalHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-host",
					Namespace: "test-ns",
				},
				Spec: metal3api.BareMetalHostSpec{
					NetworkInterfaces: []metal3api.NetworkInterface{
						{Name: "eth0"},
						{Name: "invalid"},
					},
				},
				Status: metal3api.BareMetalHostStatus{
					Provisioning: metal3api.ProvisionStatus{
						State: metal3api.StateAvailable,
					},
					HardwareDetails: &metal3api.HardwareDetails{
						NIC: []metal3api.NIC{
							{Name: "eth0", MAC: "00:11:22:33:44:55"},
						},
					},
				},
			},
			expectedDirty:          true,
			expectedValidationPass: false,
			expectedReason:         "InvalidInterfaceNames",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			r := &BareMetalHostReconciler{
				Client: fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(tc.host).Build(),
			}

			dirty, err := r.validateNetworkInterfaces(tc.host)
			assert.NoError(t, err)
			assert.Equal(t, tc.expectedDirty, dirty)

			cond := meta.FindStatusCondition(tc.host.Status.Conditions, metal3api.ConditionNetworkInterfacesValid)
			if tc.expectedValidationPass {
				assert.NotNil(t, cond)
				assert.Equal(t, metav1.ConditionTrue, cond.Status)
				assert.Equal(t, tc.expectedReason, cond.Reason)
			} else if tc.expectedReason != "" {
				assert.NotNil(t, cond)
				assert.Equal(t, metav1.ConditionFalse, cond.Status)
				assert.Equal(t, tc.expectedReason, cond.Reason)
			}
		})
	}
}

func TestSwitchPortConfigurationNeedsUpdate(t *testing.T) {
	testCases := []struct {
		name           string
		host           *metal3api.BareMetalHost
		expectedUpdate bool
		expectedReason string
	}{
		{
			name: "no-network-interfaces",
			host: &metal3api.BareMetalHost{
				Spec: metal3api.BareMetalHostSpec{},
				Status: metal3api.BareMetalHostStatus{
					AppliedNetworkInterfaces: []metal3api.NetworkInterface{
						{Name: "eth0"},
					},
				},
			},
			expectedUpdate: true,
			expectedReason: "NetworkInterfacesRemoved",
		},
		{
			name: "validation-not-passed",
			host: &metal3api.BareMetalHost{
				Spec: metal3api.BareMetalHostSpec{
					NetworkInterfaces: []metal3api.NetworkInterface{
						{Name: "eth0"},
					},
				},
				Status: metal3api.BareMetalHostStatus{
					Conditions: []metav1.Condition{
						{Type: metal3api.ConditionNetworkInterfacesValid, Status: metav1.ConditionFalse, Reason: "InvalidInterfaceNames"},
					},
				},
			},
			expectedUpdate: false,
			expectedReason: "",
		},
		{
			name: "initial-configuration",
			host: &metal3api.BareMetalHost{
				Spec: metal3api.BareMetalHostSpec{
					NetworkInterfaces: []metal3api.NetworkInterface{
						{Name: "eth0"},
					},
				},
				Status: metal3api.BareMetalHostStatus{
					Conditions: []metav1.Condition{
						{Type: metal3api.ConditionNetworkInterfacesValid, Status: metav1.ConditionTrue, Reason: "AllInterfacesValid"},
					},
				},
			},
			expectedUpdate: true,
			expectedReason: "InitialConfiguration",
		},
		{
			name: "configuration-changed",
			host: &metal3api.BareMetalHost{
				Spec: metal3api.BareMetalHostSpec{
					NetworkInterfaces: []metal3api.NetworkInterface{
						{Name: "eth0"},
						{Name: "eth1"},
					},
				},
				Status: metal3api.BareMetalHostStatus{
					Conditions: []metav1.Condition{
						{Type: metal3api.ConditionNetworkInterfacesValid, Status: metav1.ConditionTrue, Reason: "AllInterfacesValid"},
					},
					AppliedNetworkInterfaces: []metal3api.NetworkInterface{
						{Name: "eth0"},
					},
				},
			},
			expectedUpdate: true,
			expectedReason: "NetworkInterfaceSpecChanged",
		},
		{
			name: "no-changes",
			host: &metal3api.BareMetalHost{
				Spec: metal3api.BareMetalHostSpec{
					NetworkInterfaces: []metal3api.NetworkInterface{
						{Name: "eth0"},
					},
				},
				Status: metal3api.BareMetalHostStatus{
					Conditions: []metav1.Condition{
						{Type: metal3api.ConditionNetworkInterfacesValid, Status: metav1.ConditionTrue, Reason: "AllInterfacesValid"},
					},
					AppliedNetworkInterfaces: []metal3api.NetworkInterface{
						{Name: "eth0"},
					},
				},
			},
			expectedUpdate: false,
			expectedReason: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			r := &BareMetalHostReconciler{}
			needsUpdate, reason := r.switchPortConfigurationNeedsUpdate(tc.host)
			assert.Equal(t, tc.expectedUpdate, needsUpdate)
			assert.Equal(t, tc.expectedReason, reason)
		})
	}
}

func TestGetAvailableNICNames(t *testing.T) {
	testCases := []struct {
		name     string
		nics     []metal3api.NIC
		expected []string
	}{
		{
			name: "multiple-nics",
			nics: []metal3api.NIC{
				{Name: "eth2", MAC: "00:11:22:33:44:77"},
				{Name: "eth0", MAC: "00:11:22:33:44:55"},
				{Name: "eth1", MAC: "00:11:22:33:44:66"},
			},
			expected: []string{"eth0", "eth1", "eth2"},
		},
		{
			name: "empty-names-filtered",
			nics: []metal3api.NIC{
				{Name: "", MAC: "00:11:22:33:44:55"},
				{Name: "eth0", MAC: "00:11:22:33:44:66"},
			},
			expected: []string{"eth0"},
		},
		{
			name:     "empty-list",
			nics:     []metal3api.NIC{},
			expected: []string{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			r := &BareMetalHostReconciler{}
			result := r.getAvailableNICNames(tc.nics)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestResolveSwitchPortConfigs(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = metal3api.AddToScheme(scheme)

	attachment := &metal3api.HostNetworkAttachment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-attachment",
			Namespace: "test-ns",
		},
		Spec: metal3api.HostNetworkAttachmentSpec{
			Mode:         metal3api.SwitchportModeAccess,
			NativeVLAN:   100,
			AllowedVLANs: nil,
			MTU:          ptr.To(9000),
		},
	}

	host := &metal3api.BareMetalHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-host",
			Namespace: "test-ns",
		},
		Spec: metal3api.BareMetalHostSpec{
			NetworkInterfaces: []metal3api.NetworkInterface{
				{
					Name: "eth0",
					HostNetworkAttachment: metal3api.HostNetworkAttachmentRef{
						Name: "test-attachment",
					},
				},
			},
		},
	}

	r := &BareMetalHostReconciler{
		Client: fakeclient.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(host, attachment).
			Build(),
	}

	configs, err := r.resolveSwitchPortConfigs(context.TODO(), host)
	assert.NoError(t, err)
	assert.Len(t, configs, 1)
	assert.Contains(t, configs, "eth0")
	assert.Equal(t, "access", configs["eth0"].Mode)
	assert.Equal(t, 100, configs["eth0"].NativeVLAN)
	assert.Equal(t, ptr.To(9000), configs["eth0"].MTU)
}

func TestResolveSwitchPortConfigsCrossNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = metal3api.AddToScheme(scheme)

	attachment := &metal3api.HostNetworkAttachment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-attachment",
			Namespace: "other-ns",
		},
		Spec: metal3api.HostNetworkAttachmentSpec{
			Mode:       metal3api.SwitchportModeTrunk,
			NativeVLAN: 1,
			AllowedVLANs: []int{10, 20, 30},
		},
	}

	host := &metal3api.BareMetalHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-host",
			Namespace: "test-ns",
		},
		Spec: metal3api.BareMetalHostSpec{
			NetworkInterfaces: []metal3api.NetworkInterface{
				{
					Name: "eth0",
					HostNetworkAttachment: metal3api.HostNetworkAttachmentRef{
						Name:      "test-attachment",
						Namespace: "other-ns",
					},
				},
			},
		},
	}

	r := &BareMetalHostReconciler{
		Client: fakeclient.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(host, attachment).
			Build(),
	}

	configs, err := r.resolveSwitchPortConfigs(context.TODO(), host)
	assert.NoError(t, err)
	assert.Len(t, configs, 1)
	assert.Contains(t, configs, "eth0")
	assert.Equal(t, "trunk", configs["eth0"].Mode)
	assert.Equal(t, []int{10, 20, 30}, configs["eth0"].AllowedVLANs)
}

func TestResolveSwitchPortConfigsNotFound(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = metal3api.AddToScheme(scheme)

	host := &metal3api.BareMetalHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-host",
			Namespace: "test-ns",
		},
		Spec: metal3api.BareMetalHostSpec{
			NetworkInterfaces: []metal3api.NetworkInterface{
				{
					Name: "eth0",
					HostNetworkAttachment: metal3api.HostNetworkAttachmentRef{
						Name: "non-existent",
					},
				},
			},
		},
	}

	r := &BareMetalHostReconciler{
		Client: fakeclient.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(host).
			Build(),
		Log: ctrl.Log.WithName("controllers").WithName("BareMetalHost"),
	}

	// With the graceful handling of missing attachments, this should not error.
	// Instead, it should return an empty config map (the interface is skipped).
	configs, err := r.resolveSwitchPortConfigs(context.TODO(), host)
	assert.NoError(t, err)
	assert.NotNil(t, configs)
	assert.Empty(t, configs)
}

func TestResolveSwitchPortConfigsPartialSuccess(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = metal3api.AddToScheme(scheme)

	// Create one valid attachment
	attachment1 := &metal3api.HostNetworkAttachment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "valid-attachment",
			Namespace: "test-ns",
		},
		Spec: metal3api.HostNetworkAttachmentSpec{
			Mode:       metal3api.SwitchportModeAccess,
			NativeVLAN: 100,
			MTU:        ptr.To(1500),
		},
	}

	host := &metal3api.BareMetalHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-host",
			Namespace: "test-ns",
		},
		Spec: metal3api.BareMetalHostSpec{
			NetworkInterfaces: []metal3api.NetworkInterface{
				{
					Name: "eth0",
					HostNetworkAttachment: metal3api.HostNetworkAttachmentRef{
						Name: "valid-attachment",
					},
				},
				{
					Name: "eth1",
					HostNetworkAttachment: metal3api.HostNetworkAttachmentRef{
						Name: "missing-attachment",
					},
				},
			},
		},
	}

	r := &BareMetalHostReconciler{
		Client: fakeclient.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(host, attachment1).
			Build(),
		Log: ctrl.Log.WithName("controllers").WithName("BareMetalHost"),
	}

	// Should succeed and return config for eth0, skip eth1 gracefully
	configs, err := r.resolveSwitchPortConfigs(context.TODO(), host)
	assert.NoError(t, err)
	assert.NotNil(t, configs)
	assert.Len(t, configs, 1)
	assert.Contains(t, configs, "eth0")
	assert.NotContains(t, configs, "eth1")
	assert.Equal(t, "access", configs["eth0"].Mode)
	assert.Equal(t, 100, configs["eth0"].NativeVLAN)
}
