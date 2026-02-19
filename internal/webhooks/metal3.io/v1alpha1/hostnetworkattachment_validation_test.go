package webhooks

import (
	"context"
	"fmt"
	"testing"

	metal3api "github.com/metal3-io/baremetal-operator/apis/metal3.io/v1alpha1"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestValidateAttachment(t *testing.T) {
	testCases := []struct {
		name          string
		attachment    *metal3api.HostNetworkAttachment
		expectedError bool
		errorContains string
	}{
		{
			name: "valid-access-mode",
			attachment: &metal3api.HostNetworkAttachment{
				Spec: metal3api.HostNetworkAttachmentSpec{
					Mode:       metal3api.SwitchportModeAccess,
					NativeVLAN: 100,
				},
			},
			expectedError: false,
		},
		{
			name: "valid-trunk-mode-with-vlans",
			attachment: &metal3api.HostNetworkAttachment{
				Spec: metal3api.HostNetworkAttachmentSpec{
					Mode:         metal3api.SwitchportModeTrunk,
					NativeVLAN:   1,
					AllowedVLANs: []int{10, 20, 30},
				},
			},
			expectedError: false,
		},
		{
			name: "valid-hybrid-mode",
			attachment: &metal3api.HostNetworkAttachment{
				Spec: metal3api.HostNetworkAttachmentSpec{
					Mode:         metal3api.SwitchportModeHybrid,
					NativeVLAN:   100,
					AllowedVLANs: []int{200, 300},
				},
			},
			expectedError: false,
		},
		{
			name: "valid-mtu",
			attachment: &metal3api.HostNetworkAttachment{
				Spec: metal3api.HostNetworkAttachmentSpec{
					Mode:       metal3api.SwitchportModeAccess,
					NativeVLAN: 1,
					MTU:        ptr.To(9000),
				},
			},
			expectedError: false,
		},
		{
			name: "invalid-access-mode-with-allowed-vlans",
			attachment: &metal3api.HostNetworkAttachment{
				Spec: metal3api.HostNetworkAttachmentSpec{
					Mode:         metal3api.SwitchportModeAccess,
					NativeVLAN:   100,
					AllowedVLANs: []int{200},
				},
			},
			expectedError: true,
			errorContains: "allowedVlans cannot be specified for access mode",
		},
		{
			name: "invalid-vlan-id-zero",
			attachment: &metal3api.HostNetworkAttachment{
				Spec: metal3api.HostNetworkAttachmentSpec{
					Mode:       metal3api.SwitchportModeAccess,
					NativeVLAN: 0,
				},
			},
			expectedError: true,
			errorContains: "out of range",
		},
		{
			name: "invalid-vlan-id-too-high",
			attachment: &metal3api.HostNetworkAttachment{
				Spec: metal3api.HostNetworkAttachmentSpec{
					Mode:       metal3api.SwitchportModeAccess,
					NativeVLAN: 5000,
				},
			},
			expectedError: true,
			errorContains: "out of range",
		},
		{
			name: "invalid-allowed-vlan-out-of-range",
			attachment: &metal3api.HostNetworkAttachment{
				Spec: metal3api.HostNetworkAttachmentSpec{
					Mode:         metal3api.SwitchportModeTrunk,
					NativeVLAN:   1,
					AllowedVLANs: []int{10, 5000, 30},
				},
			},
			expectedError: true,
			errorContains: "out of range",
		},
		{
			name: "invalid-mtu-too-low",
			attachment: &metal3api.HostNetworkAttachment{
				Spec: metal3api.HostNetworkAttachmentSpec{
					Mode:       metal3api.SwitchportModeAccess,
					NativeVLAN: 1,
					MTU:        ptr.To(50),
				},
			},
			expectedError: true,
			errorContains: "MTU",
		},
		{
			name: "invalid-mtu-too-high",
			attachment: &metal3api.HostNetworkAttachment{
				Spec: metal3api.HostNetworkAttachmentSpec{
					Mode:       metal3api.SwitchportModeAccess,
					NativeVLAN: 1,
					MTU:        ptr.To(10000),
				},
			},
			expectedError: true,
			errorContains: "MTU",
		},
		{
			name: "invalid-switchport-mode",
			attachment: &metal3api.HostNetworkAttachment{
				Spec: metal3api.HostNetworkAttachmentSpec{
					Mode:       "invalid-mode",
					NativeVLAN: 1,
				},
			},
			expectedError: true,
			errorContains: "invalid switchport mode",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			webhook := &HostNetworkAttachment{}
			errs := webhook.validateAttachment(tc.attachment)

			if tc.expectedError {
				assert.NotEmpty(t, errs, "expected validation errors")
				if tc.errorContains != "" {
					found := false
					for _, err := range errs {
						if assert.Error(t, err) {
							errMsg := err.Error()
							if assert.NotEmpty(t, errMsg) && len(errMsg) > 0 {
								found = found || (errMsg != "" && len(errMsg) > 0)
							}
						}
					}
					assert.True(t, found, "expected error to contain: %s", tc.errorContains)
				}
			} else {
				assert.Empty(t, errs, "expected no validation errors")
			}
		})
	}
}

func TestFindBMHReferences(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = metal3api.AddToScheme(scheme)

	attachment := &metal3api.HostNetworkAttachment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-attachment",
			Namespace: "test-ns",
		},
	}

	bmhWithReference := &metal3api.BareMetalHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "host-with-ref",
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

	bmhWithoutReference := &metal3api.BareMetalHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "host-without-ref",
			Namespace: "test-ns",
		},
		Spec: metal3api.BareMetalHostSpec{},
	}

	bmhWithDifferentAttachment := &metal3api.BareMetalHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "host-different-ref",
			Namespace: "test-ns",
		},
		Spec: metal3api.BareMetalHostSpec{
			NetworkInterfaces: []metal3api.NetworkInterface{
				{
					Name: "eth0",
					HostNetworkAttachment: metal3api.HostNetworkAttachmentRef{
						Name: "other-attachment",
					},
				},
			},
		},
	}

	testCases := []struct {
		name              string
		bmhs              []metal3api.BareMetalHost
		expectedRefsCount int
		expectedRefs      []string
	}{
		{
			name:              "no-bmhs",
			bmhs:              []metal3api.BareMetalHost{},
			expectedRefsCount: 0,
		},
		{
			name:              "one-bmh-with-reference",
			bmhs:              []metal3api.BareMetalHost{*bmhWithReference},
			expectedRefsCount: 1,
			expectedRefs:      []string{"test-ns/host-with-ref[eth0]"},
		},
		{
			name:              "one-bmh-without-reference",
			bmhs:              []metal3api.BareMetalHost{*bmhWithoutReference},
			expectedRefsCount: 0,
		},
		{
			name:              "mixed-bmhs",
			bmhs:              []metal3api.BareMetalHost{*bmhWithReference, *bmhWithoutReference, *bmhWithDifferentAttachment},
			expectedRefsCount: 1,
			expectedRefs:      []string{"test-ns/host-with-ref[eth0]"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []runtime.Object{attachment}
			for i := range tc.bmhs {
				objs = append(objs, &tc.bmhs[i])
			}

			webhook := &HostNetworkAttachment{
				Client: fakeclient.NewClientBuilder().
					WithScheme(scheme).
					WithRuntimeObjects(objs...).
					WithIndex(&metal3api.BareMetalHost{}, bmhNetworkAttachmentIndexField, func(obj client.Object) []string {
						bmh := obj.(*metal3api.BareMetalHost)
						var attachments []string
						for _, iface := range bmh.Spec.NetworkInterfaces {
							if iface.HostNetworkAttachment.Name != "" {
								ns := iface.HostNetworkAttachment.Namespace
								if ns == "" {
									ns = bmh.Namespace
								}
								key := fmt.Sprintf("%s/%s", ns, iface.HostNetworkAttachment.Name)
								attachments = append(attachments, key)
							}
						}
						return attachments
					}).
					Build(),
			}

			refs, err := webhook.findBMHReferences(context.TODO(), attachment)
			assert.NoError(t, err)
			assert.Len(t, refs, tc.expectedRefsCount)
			if tc.expectedRefs != nil {
				assert.Equal(t, tc.expectedRefs, refs)
			}
		})
	}
}

func TestHNAValidateUpdate(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = metal3api.AddToScheme(scheme)

	oldAttachment := &metal3api.HostNetworkAttachment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-attachment",
			Namespace: "test-ns",
		},
		Spec: metal3api.HostNetworkAttachmentSpec{
			Mode:       metal3api.SwitchportModeAccess,
			NativeVLAN: 100,
		},
	}

	newAttachmentNoChange := &metal3api.HostNetworkAttachment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-attachment",
			Namespace: "test-ns",
		},
		Spec: metal3api.HostNetworkAttachmentSpec{
			Mode:       metal3api.SwitchportModeAccess,
			NativeVLAN: 100,
		},
	}

	newAttachmentChanged := &metal3api.HostNetworkAttachment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-attachment",
			Namespace: "test-ns",
		},
		Spec: metal3api.HostNetworkAttachmentSpec{
			Mode:       metal3api.SwitchportModeTrunk,
			NativeVLAN: 1,
		},
	}

	newAttachmentInvalid := &metal3api.HostNetworkAttachment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-attachment",
			Namespace: "test-ns",
		},
		Spec: metal3api.HostNetworkAttachmentSpec{
			Mode:       metal3api.SwitchportModeAccess,
			NativeVLAN: 5000, // Invalid VLAN
		},
	}

	bmhWithReference := &metal3api.BareMetalHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "host-with-ref",
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

	testCases := []struct {
		name          string
		oldAttachment *metal3api.HostNetworkAttachment
		newAttachment *metal3api.HostNetworkAttachment
		bmhs          []metal3api.BareMetalHost
		expectedError bool
		errorContains string
	}{
		{
			name:          "no-spec-change",
			oldAttachment: oldAttachment,
			newAttachment: newAttachmentNoChange,
			bmhs:          []metal3api.BareMetalHost{*bmhWithReference},
			expectedError: false,
		},
		{
			name:          "spec-changed-no-references",
			oldAttachment: oldAttachment,
			newAttachment: newAttachmentChanged,
			bmhs:          []metal3api.BareMetalHost{},
			expectedError: false,
		},
		{
			name:          "spec-changed-with-references",
			oldAttachment: oldAttachment,
			newAttachment: newAttachmentChanged,
			bmhs:          []metal3api.BareMetalHost{*bmhWithReference},
			expectedError: true,
			errorContains: "immutable while referenced",
		},
		{
			name:          "invalid-new-spec",
			oldAttachment: oldAttachment,
			newAttachment: newAttachmentInvalid,
			bmhs:          []metal3api.BareMetalHost{},
			expectedError: true,
			errorContains: "out of range",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []runtime.Object{tc.oldAttachment}
			for i := range tc.bmhs {
				objs = append(objs, &tc.bmhs[i])
			}

			webhook := &HostNetworkAttachment{
				Client: fakeclient.NewClientBuilder().
					WithScheme(scheme).
					WithRuntimeObjects(objs...).
					WithIndex(&metal3api.BareMetalHost{}, bmhNetworkAttachmentIndexField, func(obj client.Object) []string {
						bmh := obj.(*metal3api.BareMetalHost)
						var attachments []string
						for _, iface := range bmh.Spec.NetworkInterfaces {
							if iface.HostNetworkAttachment.Name != "" {
								ns := iface.HostNetworkAttachment.Namespace
								if ns == "" {
									ns = bmh.Namespace
								}
								key := fmt.Sprintf("%s/%s", ns, iface.HostNetworkAttachment.Name)
								attachments = append(attachments, key)
							}
						}
						return attachments
					}).
					Build(),
			}

			warnings, err := webhook.validateUpdate(context.TODO(), tc.oldAttachment, tc.newAttachment)
			_ = warnings // warnings not checked in these tests

			if tc.expectedError {
				assert.Error(t, err)
				if tc.errorContains != "" {
					assert.Contains(t, err.Error(), tc.errorContains)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestHNAValidateDelete(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = metal3api.AddToScheme(scheme)

	attachment := &metal3api.HostNetworkAttachment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-attachment",
			Namespace: "test-ns",
		},
	}

	bmhWithReference := &metal3api.BareMetalHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "host-with-ref",
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

	testCases := []struct {
		name          string
		bmhs          []metal3api.BareMetalHost
		expectedError bool
		errorContains string
	}{
		{
			name:          "no-references",
			bmhs:          []metal3api.BareMetalHost{},
			expectedError: false,
		},
		{
			name:          "with-references",
			bmhs:          []metal3api.BareMetalHost{*bmhWithReference},
			expectedError: true,
			errorContains: "cannot delete attachment while referenced",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []runtime.Object{attachment}
			for i := range tc.bmhs {
				objs = append(objs, &tc.bmhs[i])
			}

			webhook := &HostNetworkAttachment{
				Client: fakeclient.NewClientBuilder().
					WithScheme(scheme).
					WithRuntimeObjects(objs...).
					WithIndex(&metal3api.BareMetalHost{}, bmhNetworkAttachmentIndexField, func(obj client.Object) []string {
						bmh := obj.(*metal3api.BareMetalHost)
						var attachments []string
						for _, iface := range bmh.Spec.NetworkInterfaces {
							if iface.HostNetworkAttachment.Name != "" {
								ns := iface.HostNetworkAttachment.Namespace
								if ns == "" {
									ns = bmh.Namespace
								}
								key := fmt.Sprintf("%s/%s", ns, iface.HostNetworkAttachment.Name)
								attachments = append(attachments, key)
							}
						}
						return attachments
					}).
					Build(),
			}

			warnings, err := webhook.validateDelete(context.TODO(), attachment)
			_ = warnings // warnings not checked in these tests

			if tc.expectedError {
				assert.Error(t, err)
				if tc.errorContains != "" {
					assert.Contains(t, err.Error(), tc.errorContains)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidateVLANID(t *testing.T) {
	testCases := []struct {
		name          string
		vlanID        int
		expectedError bool
	}{
		{"valid-vlan-1", 1, false},
		{"valid-vlan-100", 100, false},
		{"valid-vlan-4094", 4094, false},
		{"invalid-vlan-0", 0, true},
		{"invalid-vlan-negative", -1, true},
		{"invalid-vlan-4095", 4095, true},
		{"invalid-vlan-5000", 5000, true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateVLANID(tc.vlanID)
			if tc.expectedError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidateMTU(t *testing.T) {
	testCases := []struct {
		name          string
		mtu           *int
		expectedError bool
	}{
		{"nil-mtu", nil, false},
		{"valid-mtu-68", ptr.To(68), false},
		{"valid-mtu-1500", ptr.To(1500), false},
		{"valid-mtu-9000", ptr.To(9000), false},
		{"invalid-mtu-67", ptr.To(67), true},
		{"invalid-mtu-9001", ptr.To(9001), true},
		{"invalid-mtu-negative", ptr.To(-1), true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			attachment := &metal3api.HostNetworkAttachment{
				Spec: metal3api.HostNetworkAttachmentSpec{
					MTU: tc.mtu,
				},
			}
			err := validateMTU(attachment)
			if tc.expectedError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
