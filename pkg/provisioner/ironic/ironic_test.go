package ironic

import (
	"context"
	"testing"

	metal3api "github.com/metal3-io/baremetal-operator/apis/metal3.io/v1alpha1"
	"github.com/metal3-io/baremetal-operator/pkg/hardwareutils/bmc"
	"github.com/metal3-io/baremetal-operator/pkg/provisioner"
	"github.com/metal3-io/baremetal-operator/pkg/provisioner/ironic/clients"
	// We don't use this package directly here, but need it imported
	// so it registers its test fixture with the other BMC access
	// types.
	_ "github.com/metal3-io/baremetal-operator/pkg/provisioner/ironic/testbmc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	logz "sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func init() {
	logf.SetLogger(logz.New(logz.UseDevMode(true)))
}

func newTestProvisionerFactory() ironicProvisionerFactory {
	return ironicProvisionerFactory{
		log: logf.Log,
		config: ironicConfig{
			deployKernelURL:  "http://deploy.test/ipa.kernel",
			deployRamdiskURL: "http://deploy.test/ipa.initramfs",
			deployISOURL:     "http://deploy.test/ipa.iso",
			maxBusyHosts:     20,
		},
	}
}

// A private function to construct an ironicProvisioner (rather than a
// Provisioner interface) in a consistent way for tests.
func newProvisionerWithSettings(host metal3api.BareMetalHost, bmcCreds bmc.Credentials, publisher provisioner.EventPublisher, ironicURL string, ironicAuthSettings clients.AuthConfig) (*ironicProvisioner, error) {
	hostData := provisioner.BuildHostData(host, bmcCreds)

	tlsConf := clients.TLSConfig{}
	clientIronic, err := clients.IronicClient(ironicURL, ironicAuthSettings, tlsConf)
	if err != nil {
		return nil, err
	}

	factory := newTestProvisionerFactory()
	factory.clientIronic = clientIronic
	return factory.ironicProvisioner(context.TODO(), hostData, publisher)
}

func makeHost() metal3api.BareMetalHost {
	rotational := true

	return metal3api.BareMetalHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myhost",
			Namespace: "myns",
			UID:       "27720611-e5d1-45d3-ba3a-222dcfaa4ca2",
		},
		Spec: metal3api.BareMetalHostSpec{
			BMC: metal3api.BMCDetails{
				Address: "test://test.bmc/",
			},
			Image: &metal3api.Image{
				URL: "not-empty",
			},
			Online:          true,
			HardwareProfile: "libvirt",
			RootDeviceHints: &metal3api.RootDeviceHints{
				DeviceName:         "userd_devicename",
				HCTL:               "1:2:3:4",
				Model:              "userd_model",
				Vendor:             "userd_vendor",
				SerialNumber:       "userd_serial",
				MinSizeGigabytes:   40,
				WWN:                "userd_wwn",
				WWNWithExtension:   "userd_with_extension",
				WWNVendorExtension: "userd_vendor_extension",
				Rotational:         &rotational,
			},
		},
		Status: metal3api.BareMetalHostStatus{
			Provisioning: metal3api.ProvisionStatus{
				ID: "provisioning-id",
				// Place the hints in the status field to pretend the
				// controller has already reconciled partially.
				RootDeviceHints: &metal3api.RootDeviceHints{
					DeviceName:         "userd_devicename",
					HCTL:               "1:2:3:4",
					Model:              "userd_model",
					Vendor:             "userd_vendor",
					SerialNumber:       "userd_serial",
					MinSizeGigabytes:   40,
					WWN:                "userd_wwn",
					WWNWithExtension:   "userd_with_extension",
					WWNVendorExtension: "userd_vendor_extension",
					Rotational:         &rotational,
				},
				BootMode: metal3api.UEFI,
			},
			HardwareProfile: "libvirt",
		},
	}
}

func makeHostLiveIso() (host metal3api.BareMetalHost) {
	host = makeHost()
	format := "live-iso"
	host.Spec.Image.DiskFormat = &format
	return host
}

func makeHostCustomDeploy(only bool) (host metal3api.BareMetalHost) {
	host = makeHost()
	host.Spec.CustomDeploy = &metal3api.CustomDeploy{
		Method: "install_everything",
	}
	if only {
		host.Spec.Image = nil
	}
	return host
}

// Implements provisioner.EventPublisher to swallow events for tests.
func nullEventPublisher(_, _ string) {}

func TestNewNoBMCDetails(t *testing.T) {
	// Create a host without BMC details
	host := makeHost()
	host.Spec.BMC = metal3api.BMCDetails{}

	factory := newTestProvisionerFactory()
	prov, err := factory.NewProvisioner(t.Context(), provisioner.BuildHostData(host, bmc.Credentials{}), nullEventPublisher)
	require.NoError(t, err)
	assert.NotNil(t, prov)
}

func TestSwitchPortConfigsEqual(t *testing.T) {
	mtu9000 := 9000

	tests := []struct {
		name     string
		existing any
		new      *provisioner.SwitchPortConfig
		want     bool
	}{
		{
			name:     "nil existing",
			existing: nil,
			new: &provisioner.SwitchPortConfig{
				Mode: "trunk",
			},
			want: false,
		},
		{
			name:     "wrong type",
			existing: "not a map",
			new: &provisioner.SwitchPortConfig{
				Mode: "trunk",
			},
			want: false,
		},
		{
			name: "equal basic config",
			existing: map[string]any{
				"mode": "trunk",
			},
			new: &provisioner.SwitchPortConfig{
				Mode: "trunk",
			},
			want: true,
		},
		{
			name: "different mode",
			existing: map[string]any{
				"mode": "access",
			},
			new: &provisioner.SwitchPortConfig{
				Mode: "trunk",
			},
			want: false,
		},
		{
			name: "equal with native vlan",
			existing: map[string]any{
				"mode":        "trunk",
				"native_vlan": float64(100), // JSON unmarshals to float64
			},
			new: &provisioner.SwitchPortConfig{
				Mode:       "trunk",
				NativeVLAN: 100,
			},
			want: true,
		},
		{
			name: "different native vlan",
			existing: map[string]any{
				"mode":        "trunk",
				"native_vlan": float64(100),
			},
			new: &provisioner.SwitchPortConfig{
				Mode:       "trunk",
				NativeVLAN: 200,
			},
			want: false,
		},
		{
			name: "equal with allowed vlans",
			existing: map[string]any{
				"mode":          "trunk",
				"allowed_vlans": []any{float64(100), float64(200), float64(300)},
			},
			new: &provisioner.SwitchPortConfig{
				Mode:         "trunk",
				AllowedVLANs: []int{100, 200, 300},
			},
			want: true,
		},
		{
			name: "different allowed vlans order",
			existing: map[string]any{
				"mode":          "trunk",
				"allowed_vlans": []any{float64(100), float64(200), float64(300)},
			},
			new: &provisioner.SwitchPortConfig{
				Mode:         "trunk",
				AllowedVLANs: []int{100, 300, 200}, // different order
			},
			want: false,
		},
		{
			name: "equal with mtu",
			existing: map[string]any{
				"mode": "trunk",
				"mtu":  float64(9000),
			},
			new: &provisioner.SwitchPortConfig{
				Mode: "trunk",
				MTU:  &mtu9000,
			},
			want: true,
		},
		{
			name: "different mtu",
			existing: map[string]any{
				"mode": "trunk",
				"mtu":  float64(1500),
			},
			new: &provisioner.SwitchPortConfig{
				Mode: "trunk",
				MTU:  &mtu9000,
			},
			want: false,
		},
		{
			name: "complete config equal",
			existing: map[string]any{
				"mode":          "trunk",
				"native_vlan":   float64(100),
				"allowed_vlans": []any{float64(100), float64(200), float64(300)},
				"mtu":           float64(9000),
			},
			new: &provisioner.SwitchPortConfig{
				Mode:         "trunk",
				NativeVLAN:   100,
				AllowedVLANs: []int{100, 200, 300},
				MTU:          &mtu9000,
			},
			want: true,
		},
		{
			name: "new has mtu nil, existing has no mtu",
			existing: map[string]any{
				"mode": "trunk",
			},
			new: &provisioner.SwitchPortConfig{
				Mode: "trunk",
				MTU:  nil,
			},
			want: true,
		},
		{
			name: "new has mtu nil, existing has mtu",
			existing: map[string]any{
				"mode": "trunk",
				"mtu":  float64(1500),
			},
			new: &provisioner.SwitchPortConfig{
				Mode: "trunk",
				MTU:  nil,
			},
			want: false,
		},
		{
			name: "new has empty allowed_vlans, existing has none",
			existing: map[string]any{
				"mode": "trunk",
			},
			new: &provisioner.SwitchPortConfig{
				Mode:         "trunk",
				AllowedVLANs: []int{},
			},
			want: true,
		},
		{
			name: "new has empty allowed_vlans, existing has some",
			existing: map[string]any{
				"mode":          "trunk",
				"allowed_vlans": []any{float64(100)},
			},
			new: &provisioner.SwitchPortConfig{
				Mode:         "trunk",
				AllowedVLANs: []int{},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := switchPortConfigsEqual(tt.existing, tt.new)
			assert.Equal(t, tt.want, got)
		})
	}
}
