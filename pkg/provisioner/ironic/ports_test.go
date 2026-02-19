package ironic

import (
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/baremetal/v1/ports"
	metal3api "github.com/metal3-io/baremetal-operator/apis/metal3.io/v1alpha1"
	"github.com/metal3-io/baremetal-operator/pkg/hardwareutils/bmc"
	"github.com/metal3-io/baremetal-operator/pkg/provisioner/ironic/clients"
	"github.com/metal3-io/baremetal-operator/pkg/provisioner/ironic/testserver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeduplicateNICsByMAC(t *testing.T) {
	tests := []struct {
		name     string
		nics     []metal3api.NIC
		expected int
	}{
		{
			name: "no-duplicates",
			nics: []metal3api.NIC{
				{MAC: "aa:bb:cc:dd:ee:01", Name: "eth0"},
				{MAC: "aa:bb:cc:dd:ee:02", Name: "eth1"},
			},
			expected: 2,
		},
		{
			name: "duplicate-macs-different-ips",
			nics: []metal3api.NIC{
				{MAC: "aa:bb:cc:dd:ee:01", Name: "eth0", IP: "192.168.1.1"},
				{MAC: "aa:bb:cc:dd:ee:01", Name: "eth0", IP: "fd00::1"},
				{MAC: "aa:bb:cc:dd:ee:02", Name: "eth1", IP: "192.168.1.2"},
			},
			expected: 2,
		},
		{
			name: "mixed-case-macs",
			nics: []metal3api.NIC{
				{MAC: "AA:BB:CC:DD:EE:01", Name: "eth0"},
				{MAC: "aa:bb:cc:dd:ee:01", Name: "eth0"},
			},
			expected: 1,
		},
		{
			name: "empty-mac-filtered",
			nics: []metal3api.NIC{
				{MAC: "", Name: "eth0"},
				{MAC: "aa:bb:cc:dd:ee:01", Name: "eth1"},
			},
			expected: 1,
		},
		{
			name:     "empty-list",
			nics:     []metal3api.NIC{},
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := deduplicateNICsByMAC(tt.nics)
			assert.Equal(t, tt.expected, len(result), "unexpected number of deduplicated NICs")

			// Verify no duplicates in result
			seen := make(map[string]bool)
			for _, nic := range result {
				mac := nic.MAC
				assert.False(t, seen[mac], "found duplicate MAC in result: %s", mac)
				seen[mac] = true
			}
		})
	}
}

func TestBuildLocalLinkConnection(t *testing.T) {
	tests := []struct {
		name     string
		nic      metal3api.NIC
		expected map[string]interface{}
	}{
		{
			name: "complete-lldp-data",
			nic: metal3api.NIC{
				MAC: "aa:bb:cc:dd:ee:01",
				LLDP: &metal3api.LLDP{
					SwitchID:         "00:11:22:33:44:55",
					PortID:           "Ethernet1/1",
					SwitchSystemName: "switch.example.com",
				},
			},
			expected: map[string]interface{}{
				"switch_id":   "00:11:22:33:44:55",
				"port_id":     "Ethernet1/1",
				"switch_info": "switch.example.com",
			},
		},
		{
			name: "partial-lldp-data",
			nic: metal3api.NIC{
				MAC: "aa:bb:cc:dd:ee:01",
				LLDP: &metal3api.LLDP{
					SwitchID: "00:11:22:33:44:55",
					PortID:   "Ethernet1/1",
				},
			},
			expected: map[string]interface{}{
				"switch_id": "00:11:22:33:44:55",
				"port_id":   "Ethernet1/1",
			},
		},
		{
			name: "no-lldp-data",
			nic: metal3api.NIC{
				MAC: "aa:bb:cc:dd:ee:01",
			},
			expected: nil,
		},
		{
			name: "empty-lldp-fields",
			nic: metal3api.NIC{
				MAC:  "aa:bb:cc:dd:ee:01",
				LLDP: &metal3api.LLDP{},
			},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := buildLocalLinkConnection(tt.nic)
			if tt.expected == nil {
				assert.Nil(t, result, "expected nil result")
			} else {
				assert.NotNil(t, result, "expected non-nil result")
				assert.Equal(t, len(tt.expected), len(result), "unexpected number of fields")
				for key, expectedValue := range tt.expected {
					assert.Equal(t, expectedValue, result[key], "field %s has wrong value", key)
				}
			}
		})
	}
}

func TestEnsurePortsCreatesNewPorts(t *testing.T) {
	nodeUUID := "test-node-uuid"

	// Set up ironic mock with empty port list and port creation endpoint
	ironic := testserver.NewIronic(t).
		PortDetail([]ports.Port{}).
		PortCreate(ports.Port{UUID: "new-port-uuid"})
	ironic.Start()
	defer ironic.Stop()

	host := makeHost()
	auth := clients.AuthConfig{Type: clients.NoAuth}
	prov, err := newProvisionerWithSettings(host, bmc.Credentials{}, nullEventPublisher, ironic.Endpoint(), auth)
	require.NoError(t, err)

	prov.nodeID = nodeUUID
	prov.storedHardwareDetails = &metal3api.HardwareDetails{
		NIC: []metal3api.NIC{
			{MAC: "aa:bb:cc:dd:ee:01", Name: "eth0"},
		},
	}

	err = prov.EnsurePorts()
	assert.NoError(t, err)
}

func TestEnsurePortsSkipsExistingPortWithLLDP(t *testing.T) {
	nodeUUID := "test-node-uuid"

	existingPort := ports.Port{
		UUID:     "existing-port-uuid",
		NodeUUID: nodeUUID,
		Address:  "aa:bb:cc:dd:ee:01",
		LocalLinkConnection: map[string]interface{}{
			"switch_id": "00:11:22:33:44:55",
			"port_id":   "Ethernet1/1",
		},
	}

	// Port already has LLDP data - should not be updated
	ironic := testserver.NewIronic(t).
		PortDetail([]ports.Port{existingPort})
	ironic.Start()
	defer ironic.Stop()

	host := makeHost()
	auth := clients.AuthConfig{Type: clients.NoAuth}
	prov, err := newProvisionerWithSettings(host, bmc.Credentials{}, nullEventPublisher, ironic.Endpoint(), auth)
	require.NoError(t, err)

	prov.nodeID = nodeUUID
	prov.storedHardwareDetails = &metal3api.HardwareDetails{
		NIC: []metal3api.NIC{
			{
				MAC:  "aa:bb:cc:dd:ee:01",
				Name: "eth0",
				LLDP: &metal3api.LLDP{
					SwitchID: "different-switch",
					PortID:   "different-port",
				},
			},
		},
	}

	err = prov.EnsurePorts()
	assert.NoError(t, err)
}

func TestEnsurePortsFallsBackToBootMAC(t *testing.T) {
	nodeUUID := "test-node-uuid"

	// No stored hardware details - should fall back to boot MAC only
	// Set up port for the boot MAC check
	existingPort := ports.Port{
		NodeUUID: nodeUUID,
		Address:  "11:22:33:44:55:66",
	}

	ironic := testserver.NewIronic(t).
		Port(existingPort)
	ironic.Start()
	defer ironic.Stop()

	host := makeHost()
	host.Spec.BootMACAddress = "11:22:33:44:55:66"
	auth := clients.AuthConfig{Type: clients.NoAuth}
	prov, err := newProvisionerWithSettings(host, bmc.Credentials{}, nullEventPublisher, ironic.Endpoint(), auth)
	require.NoError(t, err)

	prov.nodeID = nodeUUID
	prov.storedHardwareDetails = nil // no hardware details

	err = prov.EnsurePorts()
	assert.NoError(t, err)
}

func TestEnsurePortsNoNodeID(t *testing.T) {
	host := makeHost()
	auth := clients.AuthConfig{Type: clients.NoAuth}

	ironic := testserver.NewIronic(t)
	ironic.Start()
	defer ironic.Stop()

	prov, err := newProvisionerWithSettings(host, bmc.Credentials{}, nullEventPublisher, ironic.Endpoint(), auth)
	require.NoError(t, err)

	prov.nodeID = "" // not registered

	err = prov.EnsurePorts()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "node not registered")
}
