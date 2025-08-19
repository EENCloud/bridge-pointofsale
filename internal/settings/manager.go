package settings

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/eencloud/goeen/log"
)

// BridgePayload defines the bridge-native structure for POS configurations
type BridgePayload struct {
	SevenElevenRegisters []interface{} `json:"7eleven_registers,omitempty"`
	// Add other vendor register types here as needed
}

// ESNBridgeConfig stores bridge-native configuration for a specific ESN
type ESNBridgeConfig struct {
	ESN    string                 `json:"esn"`
	Config map[string]interface{} `json:"config"` // Raw bridge format
	Vendor *VendorConfig          `json:"vendor"` // Generated vendor config
}

// VendorConfig defines the structure for a single vendor's configuration block.
type VendorConfig struct {
	Vendor     string          `json:"vendor"`
	ListenPort int             `json:"listen_port"`
	Registers  json.RawMessage `json:"registers"`
}

// FullPayload defines the structure of the entire JSON posted from bridge-core.
type FullPayload struct {
	Vendors []VendorConfig `json:"vendors"`
}

// ESNVendorConfig stores configuration for a specific ESN
type ESNVendorConfig struct {
	ESN    string        `json:"esn"`
	Vendor *VendorConfig `json:"vendor"`
}

// Manager handles the storage and retrieval of POS configurations.
type Manager struct {
	sync.RWMutex
	logger         *log.Logger
	activeVendor   *VendorConfig
	changeChan     chan struct{}
	esnConfigs     map[string]*ESNBridgeConfig // ESN -> bridge config
	updateCallback func(esn string, vendor *VendorConfig)
}

// NewManager creates a new configuration manager.
func NewManager(logger *log.Logger) *Manager {
	return &Manager{
		logger:     logger,
		changeChan: make(chan struct{}, 1),
		esnConfigs: make(map[string]*ESNBridgeConfig),
	}
}

// UpdateSettings parses bridge-native payload and updates the active vendor config.
func (m *Manager) UpdateSettings(esn string, payload []byte) error {
	m.Lock()
	defer m.Unlock()

	// Parse as raw map first to handle bridge format
	var bridgeConfig map[string]interface{}
	if err := json.Unmarshal(payload, &bridgeConfig); err != nil {
		return fmt.Errorf("could not unmarshal bridge payload: %w", err)
	}

	// Check if payload has content (7eleven_registers)
	var hasConfig bool
	var generatedVendor *VendorConfig

	if registers, ok := bridgeConfig["7eleven_registers"]; ok {
		if registersArray, ok := registers.([]interface{}); ok && len(registersArray) > 0 {
			cleanedArray := deduplicateRegisters(registersArray)
			if len(cleanedArray) > 0 {
				hasConfig = true
				defaultPort := 6334

				registersJSON, err := json.Marshal(cleanedArray)
				if err != nil {
					return fmt.Errorf("failed to marshal registers: %w", err)
				}

				generatedVendor = &VendorConfig{
					Vendor:     "7eleven_registers",
					ListenPort: defaultPort,
					Registers:  json.RawMessage(registersJSON),
				}
			}
		}
	}

	if hasConfig {
		m.logger.Infof("Received bridge-native configuration from ESN %s", esn)

		// Store ESN-specific bridge configuration
		m.esnConfigs[esn] = &ESNBridgeConfig{
			ESN:    esn,
			Config: bridgeConfig,
			Vendor: generatedVendor,
		}

		// Activate vendor (single implementation)
		if m.activeVendor == nil {
			m.activeVendor = generatedVendor
			m.logger.Infof("Activating POS vendor from bridge config")
		}

		// Notify vendor about this ESN's configuration
		if m.updateCallback != nil {
			m.updateCallback(esn, generatedVendor)
		}

	} else {
		m.logger.Infof("ESN %s deactivating vendor configuration", esn)
		delete(m.esnConfigs, esn)

		if m.updateCallback != nil {
			m.updateCallback(esn, nil)
		}

		if len(m.esnConfigs) == 0 {
			m.logger.Info("No active ESN configurations. Deactivating vendor.")
			m.activeVendor = nil
		}
	}

	m.notifyChange()
	return nil
}

// GetActiveVendor returns a copy of the current active vendor configuration.
func (m *Manager) GetActiveVendor() *VendorConfig {
	m.RLock()
	defer m.RUnlock()

	if m.activeVendor == nil {
		return nil
	}

	vendorCopy := *m.activeVendor
	return &vendorCopy
}

// Changes returns a channel that signals when settings have been updated.
func (m *Manager) Changes() <-chan struct{} {
	return m.changeChan
}

// SetUpdateCallback sets the function to call when ESN configurations are updated
func (m *Manager) SetUpdateCallback(callback func(esn string, vendor *VendorConfig)) {
	m.Lock()
	defer m.Unlock()
	m.updateCallback = callback
}

// GetAllESNConfigurations returns all current ESN configurations for replay to new vendors
func (m *Manager) GetAllESNConfigurations() map[string]*VendorConfig {
	m.RLock()
	defer m.RUnlock()

	result := make(map[string]*VendorConfig)
	for esn, config := range m.esnConfigs {
		result[esn] = config.Vendor
	}
	return result
}

func (m *Manager) notifyChange() {
	select {
	case m.changeChan <- struct{}{}:
	default:
	}
}

func deduplicateRegisters(registers []interface{}) []interface{} {
	seen := make(map[string]bool)
	result := make([]interface{}, 0, len(registers))

	for _, reg := range registers {
		if regMap, ok := reg.(map[string]interface{}); ok {
			if ip, hasIP := regMap["ip_address"].(string); hasIP {
				if terminal, hasTerminal := regMap["terminal_number"].(string); hasTerminal {
					key := ip + ":" + terminal
					if !seen[key] {
						seen[key] = true
						result = append(result, reg)
					}
				}
			}
		}
	}

	return result
}
