package settings

import (
	"encoding/json"
	"testing"
)

func TestVendorConfig_Basic(t *testing.T) {
	config := &VendorConfig{
		ListenPort: 6334,
	}

	if config.ListenPort != 6334 {
		t.Errorf("Expected port 6334, got %d", config.ListenPort)
	}
}

func TestESNVendorConfig_Basic(t *testing.T) {
	vendor := &VendorConfig{}
	config := &ESNVendorConfig{
		ESN:    "test-esn-123",
		Vendor: vendor,
	}

	if config.ESN != "test-esn-123" {
		t.Errorf("Expected ESN 'test-esn-123', got '%s'", config.ESN)
	}

	if config.Vendor != vendor {
		t.Error("Vendor not set correctly")
	}
}

func TestFullPayload_Basic(t *testing.T) {
	vendor := VendorConfig{ListenPort: 6334}
	payload := &FullPayload{
		Vendors: []VendorConfig{vendor},
	}

	if len(payload.Vendors) != 1 {
		t.Errorf("Expected 1 vendor, got %d", len(payload.Vendors))
	}

	// brand not used anymore
}

func TestVendorConfig_JSONMarshaling(t *testing.T) {
	config := &VendorConfig{
		ListenPort: 6334,
		Registers:  json.RawMessage(`{"test": "data"}`),
	}

	data, err := json.Marshal(config)
	if err != nil {
		t.Errorf("Failed to marshal VendorConfig: %v", err)
	}

	var decoded VendorConfig
	err = json.Unmarshal(data, &decoded)
	if err != nil {
		t.Errorf("Failed to unmarshal VendorConfig: %v", err)
	}

	if decoded.ListenPort != 6334 {
		t.Errorf("Expected port 6334, got %d", decoded.ListenPort)
	}
}
