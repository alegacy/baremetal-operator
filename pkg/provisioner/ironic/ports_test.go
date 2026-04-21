package ironic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/baremetal/v1/ports"
	metal3api "github.com/metal3-io/baremetal-operator/apis/metal3.io/v1alpha1"
	"github.com/metal3-io/baremetal-operator/pkg/hardwareutils/bmc"
	"github.com/metal3-io/baremetal-operator/pkg/provisioner"
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
			assert.Len(t, result, tt.expected, "unexpected number of deduplicated NICs")

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
			result := buildLocalLinkFromNIC(tt.nic)
			if tt.expected == nil {
				assert.Nil(t, result, "expected nil result")
			} else {
				assert.NotNil(t, result, "expected non-nil result")
				assert.Len(t, result, len(tt.expected), "unexpected number of fields")
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

	err = prov.EnsurePorts(context.Background())
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

	err = prov.EnsurePorts(context.Background())
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

	err = prov.EnsurePorts(context.Background())
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

	prov.nodeID = ""                    // not registered
	prov.config.enableNetworking = true // enable to reach the nodeID check

	err = prov.EnsurePorts(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "node not registered")
}

func TestCreatePortWithLLDPData(t *testing.T) {
	nodeUUID := "test-node-uuid"
	var capturedBody map[string]interface{}

	ironic := testserver.NewIronic(t).
		PortDetail([]ports.Port{})
	ironic.Handler("/v1/ports", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &capturedBody)
			resp := ports.Port{UUID: "new-port-uuid"}
			ironic.SendJSONResponse(resp, http.StatusCreated, w, r)
			return
		}
		http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
	})
	ironic.Start()
	defer ironic.Stop()

	host := makeHost()
	auth := clients.AuthConfig{Type: clients.NoAuth}
	prov, err := newProvisionerWithSettings(host, bmc.Credentials{}, nullEventPublisher, ironic.Endpoint(), auth)
	require.NoError(t, err)

	prov.nodeID = nodeUUID
	prov.config.enableNetworking = true
	prov.storedHardwareDetails = &metal3api.HardwareDetails{
		NIC: []metal3api.NIC{
			{
				MAC:  "aa:bb:cc:dd:ee:01",
				Name: "eth0",
				LLDP: &metal3api.LLDP{
					SwitchID:         "00:11:22:33:44:55",
					PortID:           "Ethernet1/1",
					SwitchSystemName: "switch1.example.com",
				},
			},
		},
	}

	err = prov.EnsurePorts(context.Background())
	require.NoError(t, err)
	require.NotNil(t, capturedBody)

	llc, ok := capturedBody["local_link_connection"].(map[string]interface{})
	require.True(t, ok, "expected local_link_connection in port create request")
	assert.Equal(t, "00:11:22:33:44:55", llc["switch_id"])
	assert.Equal(t, "Ethernet1/1", llc["port_id"])
	assert.Equal(t, "switch1.example.com", llc["switch_info"])
}

func TestCreatePortWithManualLLCOverride(t *testing.T) {
	nodeUUID := "test-node-uuid"
	var capturedBody map[string]interface{}

	ironic := testserver.NewIronic(t).
		PortDetail([]ports.Port{})
	ironic.Handler("/v1/ports", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &capturedBody)
			resp := ports.Port{UUID: "new-port-uuid"}
			ironic.SendJSONResponse(resp, http.StatusCreated, w, r)
			return
		}
		http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
	})
	ironic.Start()
	defer ironic.Stop()

	host := makeHost()
	auth := clients.AuthConfig{Type: clients.NoAuth}
	prov, err := newProvisionerWithSettings(host, bmc.Credentials{}, nullEventPublisher, ironic.Endpoint(), auth)
	require.NoError(t, err)

	prov.nodeID = nodeUUID
	prov.config.enableNetworking = true
	prov.storedHardwareDetails = &metal3api.HardwareDetails{
		NIC: []metal3api.NIC{
			{
				MAC:  "aa:bb:cc:dd:ee:01",
				Name: "eth0",
				LLDP: &metal3api.LLDP{
					SwitchID: "lldp-switch",
					PortID:   "lldp-port",
				},
			},
		},
	}
	prov.portConfigs = map[string]*provisioner.PortConfig{
		"aa:bb:cc:dd:ee:01": {
			SwitchPortConfig:    provisioner.SwitchPortConfig{Mode: "access", NativeVLAN: 100},
			LocalLinkConnection: &provisioner.LocalLinkConnection{SwitchID: "manual-switch", PortID: "manual-port"},
		},
	}

	err = prov.EnsurePorts(context.Background())
	require.NoError(t, err)
	require.NotNil(t, capturedBody)

	llc, ok := capturedBody["local_link_connection"].(map[string]interface{})
	require.True(t, ok, "expected local_link_connection in port create request")
	assert.Equal(t, "manual-switch", llc["switch_id"], "manual override should take precedence over LLDP")
	assert.Equal(t, "manual-port", llc["port_id"], "manual override should take precedence over LLDP")
}

func TestCreatePortWithSwitchportExtra(t *testing.T) {
	nodeUUID := "test-node-uuid"
	var capturedBody map[string]interface{}

	ironic := testserver.NewIronic(t).
		PortDetail([]ports.Port{})
	ironic.Handler("/v1/ports", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &capturedBody)
			resp := ports.Port{UUID: "new-port-uuid"}
			ironic.SendJSONResponse(resp, http.StatusCreated, w, r)
			return
		}
		http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
	})
	ironic.Start()
	defer ironic.Stop()

	host := makeHost()
	auth := clients.AuthConfig{Type: clients.NoAuth}
	prov, err := newProvisionerWithSettings(host, bmc.Credentials{}, nullEventPublisher, ironic.Endpoint(), auth)
	require.NoError(t, err)

	prov.nodeID = nodeUUID
	prov.config.enableNetworking = true
	prov.storedHardwareDetails = &metal3api.HardwareDetails{
		NIC: []metal3api.NIC{
			{MAC: "aa:bb:cc:dd:ee:01", Name: "eth0"},
		},
	}
	prov.portConfigs = map[string]*provisioner.PortConfig{
		"aa:bb:cc:dd:ee:01": {
			SwitchPortConfig: provisioner.SwitchPortConfig{Mode: "trunk", NativeVLAN: 1, AllowedVLANs: []int{10, 20}},
		},
	}

	err = prov.EnsurePorts(context.Background())
	require.NoError(t, err)
	require.NotNil(t, capturedBody)

	extra, ok := capturedBody["extra"].(map[string]interface{})
	require.True(t, ok, "expected extra in port create request")
	switchport, ok := extra["switchport"].(map[string]interface{})
	require.True(t, ok, "expected extra.switchport in port create request")
	assert.Equal(t, "trunk", switchport["mode"])
	assert.InDelta(t, 1, switchport["native_vlan"], 0)
}

func TestUpdatePortLLDPFallback(t *testing.T) {
	nodeUUID := "test-node-uuid"
	var capturedUpdateBody []interface{}

	existingPort := ports.Port{
		UUID:                "existing-port-uuid",
		NodeUUID:            nodeUUID,
		Address:             "aa:bb:cc:dd:ee:01",
		LocalLinkConnection: map[string]interface{}{},
	}

	ironic := testserver.NewIronic(t).
		PortDetail([]ports.Port{existingPort})
	ironic.Handler("/v1/ports/existing-port-uuid", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &capturedUpdateBody)
			resp := existingPort
			ironic.SendJSONResponse(resp, http.StatusOK, w, r)
			return
		}
		http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
	})
	ironic.Start()
	defer ironic.Stop()

	host := makeHost()
	auth := clients.AuthConfig{Type: clients.NoAuth}
	prov, err := newProvisionerWithSettings(host, bmc.Credentials{}, nullEventPublisher, ironic.Endpoint(), auth)
	require.NoError(t, err)

	prov.nodeID = nodeUUID
	prov.config.enableNetworking = true
	prov.storedHardwareDetails = &metal3api.HardwareDetails{
		NIC: []metal3api.NIC{
			{
				MAC:  "aa:bb:cc:dd:ee:01",
				Name: "eth0",
				LLDP: &metal3api.LLDP{
					SwitchID: "lldp-switch-id",
					PortID:   "lldp-port-id",
				},
			},
		},
	}
	prov.portConfigs = map[string]*provisioner.PortConfig{
		"aa:bb:cc:dd:ee:01": {
			SwitchPortConfig: provisioner.SwitchPortConfig{Mode: "access", NativeVLAN: 100},
		},
	}

	err = prov.EnsurePorts(context.Background())
	require.NoError(t, err)
	require.NotNil(t, capturedUpdateBody, "expected PATCH request to update port")

	var foundLLC bool
	for _, op := range capturedUpdateBody {
		opMap, ok := op.(map[string]interface{})
		if !ok {
			continue
		}
		if opMap["path"] == "/local_link_connection" {
			foundLLC = true
			value, ok := opMap["value"].(map[string]interface{})
			require.True(t, ok)
			assert.Equal(t, "lldp-switch-id", value["switch_id"])
			assert.Equal(t, "lldp-port-id", value["port_id"])
		}
	}
	assert.True(t, foundLLC, "expected PATCH operation to set local_link_connection from LLDP data")
}

func TestEnsurePortsMultipleNICs(t *testing.T) {
	nodeUUID := "test-node-uuid"
	var createdPorts []map[string]interface{}

	ironic := testserver.NewIronic(t).
		PortDetail([]ports.Port{})
	ironic.Handler("/v1/ports", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]interface{}
			_ = json.Unmarshal(body, &parsed)
			createdPorts = append(createdPorts, parsed)
			resp := ports.Port{UUID: fmt.Sprintf("port-%d", len(createdPorts))}
			ironic.SendJSONResponse(resp, http.StatusCreated, w, r)
			return
		}
		http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
	})
	ironic.Start()
	defer ironic.Stop()

	host := makeHost()
	auth := clients.AuthConfig{Type: clients.NoAuth}
	prov, err := newProvisionerWithSettings(host, bmc.Credentials{}, nullEventPublisher, ironic.Endpoint(), auth)
	require.NoError(t, err)

	prov.nodeID = nodeUUID
	prov.config.enableNetworking = true
	prov.storedHardwareDetails = &metal3api.HardwareDetails{
		NIC: []metal3api.NIC{
			{
				MAC:  "aa:bb:cc:dd:ee:01",
				Name: "eth0",
				LLDP: &metal3api.LLDP{
					SwitchID: "switch-01",
					PortID:   "Ethernet1/1",
				},
			},
			{
				MAC:  "aa:bb:cc:dd:ee:02",
				Name: "eth1",
				LLDP: &metal3api.LLDP{
					SwitchID: "switch-02",
					PortID:   "Ethernet1/2",
				},
			},
			{
				MAC:  "aa:bb:cc:dd:ee:03",
				Name: "eth2",
			},
		},
	}
	prov.portConfigs = map[string]*provisioner.PortConfig{
		"aa:bb:cc:dd:ee:01": {
			SwitchPortConfig:    provisioner.SwitchPortConfig{Mode: "access", NativeVLAN: 100},
			LocalLinkConnection: &provisioner.LocalLinkConnection{SwitchID: "manual-switch", PortID: "manual-port"},
		},
	}

	err = prov.EnsurePorts(context.Background())
	require.NoError(t, err)
	assert.Len(t, createdPorts, 3, "expected 3 ports to be created")
}

func TestEnsurePortsPXEFlag(t *testing.T) {
	nodeUUID := "test-node-uuid"
	var createdPorts []map[string]interface{}

	ironic := testserver.NewIronic(t).
		PortDetail([]ports.Port{})
	ironic.Handler("/v1/ports", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]interface{}
			_ = json.Unmarshal(body, &parsed)
			createdPorts = append(createdPorts, parsed)
			resp := ports.Port{UUID: fmt.Sprintf("port-%d", len(createdPorts))}
			ironic.SendJSONResponse(resp, http.StatusCreated, w, r)
			return
		}
		http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
	})
	ironic.Start()
	defer ironic.Stop()

	host := makeHost()
	host.Spec.BootMACAddress = "aa:bb:cc:dd:ee:02"
	auth := clients.AuthConfig{Type: clients.NoAuth}
	prov, err := newProvisionerWithSettings(host, bmc.Credentials{}, nullEventPublisher, ironic.Endpoint(), auth)
	require.NoError(t, err)

	prov.nodeID = nodeUUID
	prov.config.enableNetworking = true
	prov.storedHardwareDetails = &metal3api.HardwareDetails{
		NIC: []metal3api.NIC{
			{
				MAC:  "aa:bb:cc:dd:ee:01",
				Name: "eth0",
				PXE:  true, // PXE flag set explicitly
			},
			{
				MAC:  "aa:bb:cc:dd:ee:02",
				Name: "eth1",
				PXE:  false, // Not PXE, but matches bootMACAddress
			},
			{
				MAC:  "aa:bb:cc:dd:ee:03",
				Name: "eth2",
				PXE:  false, // Not PXE, not bootMACAddress
			},
		},
	}

	err = prov.EnsurePorts(context.Background())
	require.NoError(t, err)
	require.Len(t, createdPorts, 3, "expected 3 ports to be created")

	// Build a map of MAC -> pxe_enabled for verification
	pxeByMAC := make(map[string]bool)
	for _, p := range createdPorts {
		mac, _ := p["address"].(string)
		pxe, _ := p["pxe_enabled"].(bool)
		pxeByMAC[mac] = pxe
	}

	assert.True(t, pxeByMAC["aa:bb:cc:dd:ee:01"], "eth0 should have PXE enabled (nic.PXE=true)")
	assert.True(t, pxeByMAC["aa:bb:cc:dd:ee:02"], "eth1 should have PXE enabled (matches bootMACAddress)")
	assert.False(t, pxeByMAC["aa:bb:cc:dd:ee:03"], "eth2 should not have PXE enabled")
}

func TestEnsurePortsPartialFailure(t *testing.T) {
	nodeUUID := "test-node-uuid"
	var callCount atomic.Int32

	ironic := testserver.NewIronic(t).
		PortDetail([]ports.Port{})
	ironic.Handler("/v1/ports", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			n := callCount.Add(1)
			if n == 1 {
				// Fail the first port creation
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			resp := ports.Port{UUID: fmt.Sprintf("port-%d", n)}
			ironic.SendJSONResponse(resp, http.StatusCreated, w, r)
			return
		}
		http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
	})
	ironic.Start()
	defer ironic.Stop()

	host := makeHost()
	auth := clients.AuthConfig{Type: clients.NoAuth}
	prov, err := newProvisionerWithSettings(host, bmc.Credentials{}, nullEventPublisher, ironic.Endpoint(), auth)
	require.NoError(t, err)

	prov.nodeID = nodeUUID
	prov.config.enableNetworking = true
	prov.storedHardwareDetails = &metal3api.HardwareDetails{
		NIC: []metal3api.NIC{
			{MAC: "aa:bb:cc:dd:ee:01", Name: "eth0"},
			{MAC: "aa:bb:cc:dd:ee:02", Name: "eth1"},
		},
	}

	err = prov.EnsurePorts(context.Background())
	require.Error(t, err, "should return an error when a port creation fails")
	assert.Contains(t, err.Error(), "failed to ensure")

	// Both ports should have been attempted (ensurePorts continues on failure)
	assert.Equal(t, int32(2), callCount.Load(), "both port creations should have been attempted")
}
