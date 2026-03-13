//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// ironicPort represents an Ironic port from the API.
//
//nolint:tagliatelle
type ironicPort struct {
	UUID                string                 `json:"uuid"`
	Address             string                 `json:"address"`
	NodeUUID            string                 `json:"node_uuid"`
	PXEEnabled          bool                   `json:"pxe_enabled"`
	Extra               map[string]interface{} `json:"extra"`
	LocalLinkConnection map[string]interface{} `json:"local_link_connection"`
}

// ironicPortsResponse represents the JSON response from the Ironic ports API.
type ironicPortsResponse struct {
	Ports []ironicPort `json:"ports"`
}

// fetchIronicPorts queries the Ironic API and returns ports for the given node.
func fetchIronicPorts(e2eConfig *Config, namespace, bmhName string) ([]ironicPort, error) {
	ironicProvisioningIP := e2eConfig.GetVariable("IRONIC_PROVISIONING_IP")
	ironicProvisioningPort := e2eConfig.GetVariable("IRONIC_PROVISIONING_PORT")
	nodeName := fmt.Sprintf("%s~%s", namespace, bmhName)
	ironicURL := fmt.Sprintf("https://%s/v1/ports/detail?node=%s",
		net.JoinHostPort(ironicProvisioningIP, ironicProvisioningPort), nodeName)
	username := e2eConfig.GetVariable("IRONIC_USERNAME")
	password := e2eConfig.GetVariable("IRONIC_PASSWORD")

	// Create HTTP client with TLS settings
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true, // #nosec G402 Skip verification as we are using self-signed certificates
	}
	httpClient := &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsConfig},
	}

	// Create the request
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ironicURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request for ironic ports: %w", err)
	}

	// Set basic auth and API version headers
	auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	req.Header.Add("Authorization", "Basic "+auth)
	req.Header.Add("X-OpenStack-Ironic-API-Version", "1.89")

	// Make the request
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request for ironic ports: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code when fetching ironic ports: %d", resp.StatusCode)
	}

	// Read the response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read ironic ports response body: %w", err)
	}

	// Parse the JSON response
	var portsResp ironicPortsResponse
	if err := json.Unmarshal(body, &portsResp); err != nil {
		return nil, fmt.Errorf("failed to parse ironic ports response: %w", err)
	}

	return portsResp.Ports, nil
}

// findPortByMAC returns the first Ironic port matching the given MAC address (case-insensitive).
func findPortByMAC(ports []ironicPort, mac string) *ironicPort {
	for i := range ports {
		if strings.EqualFold(ports[i].Address, mac) {
			return &ports[i]
		}
	}
	return nil
}
