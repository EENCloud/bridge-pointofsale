package seveneleven

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eencloud/goeen/log"
)

type Simulator struct {
	logger          *log.Logger
	targetURL       string
	dataDir         string
	trafficIPs      []string
	ipToTerminal    map[string]string
	ipToStoreNumber map[string]string
	stopChan        chan struct{}
	stopped         bool
	stopMutex       sync.Mutex

	// Modulo simulation options
	realTimeMode bool // Controls timestamp fudging only (NEVER modify transaction sequence numbers) - set via SIM_REAL_TIME
}

// NewSimulatorWithMappings creates a simulator with IP-to-terminal/store mappings
func NewSimulatorWithMappings(logger *log.Logger, targetURL string, trafficIPs []string, logFile string, realTime bool, registers []RegisterConfig) *Simulator {
	// Build IP-to-terminal and IP-to-store mappings from registers
	ipToTerminal, ipToStoreNumber := map[string]string{}, map[string]string{}
	for _, reg := range registers {
		if reg.IPAddress != "" {
			ipToTerminal[reg.IPAddress] = reg.TerminalNumber
			ipToStoreNumber[reg.IPAddress] = reg.StoreNumber
		}
	}

	return &Simulator{
		logger:          logger,
		targetURL:       targetURL,
		dataDir:         "data/7eleven/register_logs/extracted_jsonl",
		trafficIPs:      trafficIPs,
		ipToTerminal:    ipToTerminal,
		ipToStoreNumber: ipToStoreNumber,
		stopChan:        make(chan struct{}),
		stopped:         false,
		stopMutex:       sync.Mutex{},
		realTimeMode:    realTime,
	}
}

func (s *Simulator) Start() error {
	s.stopMutex.Lock()
	if s.stopped {
		s.stopMutex.Unlock()
		return fmt.Errorf("simulator already stopped")
	}
	s.stopMutex.Unlock()

	// Modulo-only mode: Each IP uses register file based on its terminal number
	s.logger.Infof("Starting modulo simulator: real_time=%t", s.realTimeMode)

	registerFiles := []string{
		"register_1.jsonl", "register_2.jsonl", "register_3.jsonl",
		"register_4.jsonl", "register_5.jsonl",
	}

	for _, sourceIP := range s.trafficIPs {
		// Get terminal number for this IP
		terminalNumber := s.ipToTerminal[sourceIP]
		if terminalNumber == "" {
			s.logger.Errorf("No terminal number found for IP %s", sourceIP)
			continue
		}

		// Calculate modulo assignment: terminal number -> register file
		if terminalNum, err := strconv.Atoi(terminalNumber); err == nil {
			fileIndex := (terminalNum - 1) % 5 // terminals 1-5 map to indices 0-4
			if fileIndex < 0 {
				fileIndex = 0 // Handle negative numbers
			}
			assignedFile := registerFiles[fileIndex]

			s.logger.Infof("IP %s (terminal %s) -> %s", sourceIP, terminalNumber, assignedFile)
			go s.simulateRegisterUnified(assignedFile, sourceIP)
		} else {
			s.logger.Errorf("Invalid terminal number '%s' for IP %s", terminalNumber, sourceIP)
		}
	}

	// Wait for stop signal
	<-s.stopChan
	s.logger.Info("Simulator stopped")
	return nil
}

// simulateRegisterUnified handles unified simulation with assigned log file and SIM_REAL_TIME control
func (s *Simulator) simulateRegisterUnified(filename, sourceIP string) {
	filePath := filepath.Join(s.dataDir, filename)
	s.logger.Infof("Starting unified simulation: %s from IP %s (real_time=%t)", filename, sourceIP, s.realTimeMode)

	for {
		// Check if we should stop before starting a new file cycle
		select {
		case <-s.stopChan:
			s.logger.Infof("Stopping unified simulation for %s from IP %s", filename, sourceIP)
			return
		default:
		}

		file, err := os.Open(filePath)
		if err != nil {
			s.logger.Errorf("Failed to open %s: %v", filename, err)
			return
		}

		scanner := bufio.NewScanner(file)
		lineCount := 0

		for scanner.Scan() {
			// Check if we should stop
			select {
			case <-s.stopChan:
				_ = file.Close()
				s.logger.Infof("Stopping unified simulation for %s from IP %s", filename, sourceIP)
				return
			default:
			}

			line := scanner.Text()
			if line == "" {
				continue
			}

			if err := s.postJSONUnified(line, sourceIP); err != nil {
				s.logger.Errorf("Failed to post JSON from %s: %v", filename, err)
			} else {
				lineCount++
				if lineCount%50 == 0 {
					s.logger.Debugf("%s: Posted %d JSON events", filename, lineCount)
				}
			}

			// Sleep with early exit on stop signal
			select {
			case <-s.stopChan:
				_ = file.Close()
				s.logger.Infof("Stopping unified simulation for %s from IP %s", filename, sourceIP)
				return
			case <-time.After(1000 * time.Millisecond):
			}
		}

		_ = file.Close()

		if err := scanner.Err(); err != nil {
			s.logger.Errorf("Error reading %s: %v", filename, err)
			return
		}

		s.logger.Debugf("Completed cycle for %s - %d events posted, restarting...", filename, lineCount)
	}
}

// postJSONUnified posts JSON with SIM_REAL_TIME control over fudging
func (s *Simulator) postJSONUnified(jsonData, sourceIP string) error {
	maxRetries := 5
	initialDelay := 100 * time.Millisecond

	// Parse JSON for potential modification
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(jsonData), &data); err == nil {
		if s.realTimeMode {
			// SIM_REAL_TIME=true: Apply fudging (terminal/store numbers and timestamps only)
			if metaData, ok := data["metaData"].(map[string]interface{}); ok {
				// Override terminal number and store number based on IP
				if terminalNumber, exists := s.ipToTerminal[sourceIP]; exists {
					metaData["terminalNumber"] = terminalNumber
				}
				if storeNumber, exists := s.ipToStoreNumber[sourceIP]; exists {
					metaData["storeNumber"] = storeNumber
				}
			}

			// Update ALL timestamp fields to current time (recursive search)
			s.updateAllTimestamps(data)
		}
		// SIM_REAL_TIME=false: No fudging at all, preserve all original data exactly as-is

		// Re-marshal the (possibly modified) JSON
		if modifiedJSON, err := json.Marshal(data); err == nil {
			jsonData = string(modifiedJSON)
		}
	}

	targetURL := s.targetURL + "/" + sourceIP

	for i := 0; i < maxRetries; i++ {
		resp, err := http.Post(targetURL, "application/json", bytes.NewBufferString(jsonData))
		if err == nil {
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
			}
			return nil
		}

		s.logger.Errorf("Attempt %d to post JSON failed: %v. Retrying in %v...", i+1, err, initialDelay*(time.Duration(1<<i)))
		time.Sleep(initialDelay * (time.Duration(1 << i)))
	}
	return fmt.Errorf("failed to post JSON after %d retries", maxRetries)
}

func (s *Simulator) updateAllTimestamps(data interface{}) {
	switch v := data.(type) {
	case map[string]interface{}:
		for k, val := range v {
			if k == "timeStamp" {
				if _, isString := val.(string); isString {
					now := time.Now()
					v[k] = now.Format("2006-01-02T15:04:05")
				}
			} else {
				s.updateAllTimestamps(val)
			}
		}
	case []interface{}:
		for _, val := range v {
			s.updateAllTimestamps(val)
		}
	}
}

// Stop gracefully stops the simulator
func (s *Simulator) Stop() {
	s.stopMutex.Lock()
	defer s.stopMutex.Unlock()

	if s.stopped {
		return
	}

	s.logger.Info("Stopping simulator...")
	s.stopped = true
	close(s.stopChan)
}

// DecodeTransactionTimestamp extracts the timestamp from a generated transaction sequence
// Format: "original-timestamp-random" -> "3475-65a1b2c3-a1b2"
func DecodeTransactionTimestamp(txnSeq string) (time.Time, error) {
	parts := strings.Split(txnSeq, "-")
	if len(parts) < 2 {
		return time.Time{}, fmt.Errorf("invalid transaction sequence format: %s", txnSeq)
	}

	timestampHex := parts[1]
	timestamp, err := strconv.ParseInt(timestampHex, 16, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to parse timestamp from %s: %v", timestampHex, err)
	}

	return time.Unix(timestamp, 0), nil
}
