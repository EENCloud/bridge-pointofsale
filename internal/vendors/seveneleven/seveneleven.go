package seveneleven

import (
	"bridge-pointofsale/internal/core"
	"bridge-pointofsale/internal/settings"
	"bridge-pointofsale/internal/vendors"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"bytes"

	goeen_log "github.com/eencloud/goeen/log"
)

const VendorName = "7eleven_registers"

// LogEndpointPayload shows what was actually served to endpoints
func LogEndpointPayload(result ETagResult) string {
	if len(result.ANNTData) == 0 {
		return "EMPTY_PAYLOAD"
	}

	if json.Valid(result.ANNTData) {
		var prettyJSON bytes.Buffer
		if err := json.Indent(&prettyJSON, result.ANNTData, "", "  "); err == nil {
			return prettyJSON.String()
		}
	}

	return fmt.Sprintf("INVALID_ANNT_FORMAT (%d bytes)", len(result.ANNTData))
}

// LogStorageCompact creates brief storage confirmation
func LogStorageCompact(result ETagResult) string {
	status := ""
	if result.Status != nil {
		status = fmt.Sprintf(" status:%s", string(*result.Status))
	}

	txnInfo := ""
	if result.TransactionSeq != "" {
		txnInfo = fmt.Sprintf(" txn:%s", result.TransactionSeq)
	}

	return fmt.Sprintf("[NS%d] %s%s%s %db",
		result.Namespace, result.RegisterIP, status, txnInfo, len(result.ANNTData))
}

type Settings struct {
	ListenPort int             `json:"listen_port"`
	Registers  json.RawMessage `json:"registers"`
}

func init() {
	vendors.Register(VendorName, New)
}

type Vendor struct {
	logger       *goeen_log.Logger
	settings     Settings
	ingestor     *Ingestor
	processor    *Processor
	simulator    *Simulator
	anntStore    *core.ANNTStore
	stateMachine *IPBasedStateMachine
	ns91Queue    chan ETagResult
	ns92Queue    chan ETagResult
	currentIPs   []string
	isSimMode    bool

	// Unified simulation: ESN to register file mapping
	esnToRegisterFile   map[string]string
	registerFileCounter int
	assignmentMutex     sync.Mutex

	// Operational metrics
	httpRequestCount int64
	lastStatusReport time.Time
	statusMutex      sync.RWMutex
}

func New(logger *goeen_log.Logger, rawConfig json.RawMessage, anntStore *core.ANNTStore) (vendors.Vendor, error) {
	var s Settings
	if err := json.Unmarshal(rawConfig, &s); err != nil {
		return nil, err
	}

	// Create unbounded channels for maximum throughput
	ns91Queue := make(chan ETagResult, 50000) // Large but not unlimited for memory safety
	ns92Queue := make(chan ETagResult, 50000) // Large but not unlimited for memory safety
	stateMachine := NewIPBasedStateMachine(ns91Queue, ns92Queue, logger)

	// Create processor and set it on the state machine
	processor := NewProcessor(logger)
	processorAdapter := NewProcessorAdapter(logger)
	stateMachine.SetProcessor(processorAdapter)
	isSimMode := os.Getenv("MODE") == "simulation"
	stateMachine.SetIPESNMappings(map[string]string{})

	return &Vendor{
		logger:              logger,
		settings:            s,
		processor:           processor,
		anntStore:           anntStore,
		stateMachine:        stateMachine,
		ns91Queue:           ns91Queue,
		ns92Queue:           ns92Queue,
		currentIPs:          []string{},
		isSimMode:           isSimMode,
		esnToRegisterFile:   make(map[string]string),
		registerFileCounter: 0,
		httpRequestCount:    0,
		lastStatusReport:    time.Now(),
	}, nil
}

func (v *Vendor) Name() string {
	return VendorName
}

func (v *Vendor) GetIPESNMappings() map[string]string {
	if v.stateMachine == nil {
		return make(map[string]string)
	}
	return v.stateMachine.GetIPESNMappings()
}

func (v *Vendor) GetResolvedConfig() map[string]interface{} {
	result := make(map[string]interface{})
	result["vendor"] = VendorName
	result["listen_port"] = v.settings.ListenPort
	if v.isSimMode {
		result["mode"] = "simulation"
	} else {
		result["mode"] = "production"
	}
	return result
}

func (v *Vendor) HandleESNConfiguration(esn string, vendor *settings.VendorConfig) {
	v.logger.Infof("POS CONFIG CHANGE: ESN %s received new configuration", esn)

	var registers []RegisterConfig
	if err := json.Unmarshal(vendor.Registers, &registers); err != nil {
		v.logger.Errorf("Failed to parse registers for ESN %s: %v", esn, err)
		return
	}

	v.logger.Infof("Configured %d registers for ESN %s", len(registers), esn)

	if v.ingestor != nil {
		v.ingestor.AddESNRegisters(esn, registers)
		for _, reg := range registers {
			v.stateMachine.AddIPESNMapping(reg.IPAddress, esn)
			v.logger.Infof("Register mapped: %s (term %s) → ESN %s", reg.IPAddress, reg.TerminalNumber, esn)
		}

		if v.isSimMode {
			// Assign register file for this ESN using modulo logic
			v.assignRegisterFileForESN(esn, registers)

			// Start or restart simulator with all current mappings
			v.updateSimulator()
		}
	}
}

// assignRegisterFileForESN assigns a register file to an ESN using terminal number modulo 5
func (v *Vendor) assignRegisterFileForESN(esn string, registers []RegisterConfig) string {
	v.assignmentMutex.Lock()
	defer v.assignmentMutex.Unlock()

	// Check if ESN already has an assignment
	if existingFile, exists := v.esnToRegisterFile[esn]; exists {
		v.logger.Infof("ESN %s already assigned to %s", esn, existingFile)
		return existingFile
	}

	// Available register files
	registerFiles := []string{
		"register_1.jsonl", "register_2.jsonl", "register_3.jsonl",
		"register_4.jsonl", "register_5.jsonl",
	}

	// Assign file based on terminal number modulo 5
	var assignedFile string
	if len(registers) > 0 && registers[0].TerminalNumber != "" {
		// Parse terminal number and use modulo 5
		if terminalNum, err := strconv.Atoi(registers[0].TerminalNumber); err == nil {
			fileIndex := (terminalNum - 1) % 5 // terminals 1-5 map to indices 0-4
			if fileIndex < 0 {
				fileIndex = 0 // Handle negative numbers
			}
			assignedFile = registerFiles[fileIndex]
			v.logger.Infof("Assigned ESN %s → %s (terminal %s, modulo assignment)", esn, assignedFile, registers[0].TerminalNumber)
		} else {
			// Fallback to cycling if terminal number is invalid
			assignedFile = registerFiles[v.registerFileCounter%len(registerFiles)]
			v.registerFileCounter++
			v.logger.Infof("Assigned ESN %s → %s (fallback, invalid terminal: %s)", esn, assignedFile, registers[0].TerminalNumber)
		}
	} else {
		// Fallback to cycling if no terminal number available
		assignedFile = registerFiles[v.registerFileCounter%len(registerFiles)]
		v.registerFileCounter++
		v.logger.Infof("Assigned ESN %s → %s (fallback, no terminal number)", esn, assignedFile)
	}

	v.esnToRegisterFile[esn] = assignedFile
	return assignedFile
}

func (v *Vendor) Start() error {
	if v.isSimMode {
		realTime := os.Getenv("REAL_TIME")
		if realTime == "false" {
			v.logger.Info("Simulation mode: waiting for pos_config (REAL_TIME=false, no fudging)")
		} else {
			v.logger.Info("Simulation mode: waiting for pos_config (REAL_TIME=true, with fudging)")
		}
		v.settings.ListenPort = 6334
	} else {
		v.logger.Info("Non-simulation mode: Waiting for external test scripts")
	}

	v.logger.Infof("Starting 7eleven_registers vendor services on port %d (verbose: %s)...",
		v.settings.ListenPort, os.Getenv("POS_VERBOSE_LOGGING"))
	// For now, create a placeholder channel until we refactor ingestor
	placeholderQueue := make(chan []byte, 1000)
	v.ingestor = NewIngestor(v.logger, v.settings, v.processor, placeholderQueue, v.stateMachine)

	go func() {
		if err := v.ingestor.Start(); err != nil {
			v.logger.Errorf("7-Eleven ingestor failed: %v", err)
		}
	}()

	go v.processStateResults()
	go v.statusReporter()

	return nil
}

func (v *Vendor) processStateResults() {
	for {
		select {
		case result := <-v.ns91Queue:
			// Convert ETagResult to ANNTItem
			var anntData map[string]interface{}
			if result.ANNTData != nil {
				if err := json.Unmarshal(result.ANNTData, &anntData); err != nil {
					v.logger.Errorf("Failed to unmarshal ANNTData to ANNT: %v", err)
					continue
				}

			}

			anntItem := core.ANNTItem{
				ID:             result.ID,
				RegisterIP:     result.RegisterIP,
				TransactionSeq: result.TransactionSeq,
				ANNTData:       anntData,
				Namespace:      result.Namespace,
				Priority:       1,
				CreatedAt:      result.CreatedAt,
				ESN:            result.ESN,
			}

			if err := v.anntStore.StoreANNT(anntItem); err != nil {
				v.logger.Errorf("Failed to store NS91 ANNT: %v", err)
			} else if v.isSimMode {
				v.logger.Debugf("NS91 Stored: %s", LogStorageCompact(result))
			}

		case result := <-v.ns92Queue:
			if v.isSimMode {
				if os.Getenv("POS_VERBOSE_LOGGING") != "false" {
					v.logger.Debugf("NS92 Sent to Bridge - Payload:\n%s", LogEndpointPayload(result))
				} else {
					v.logger.Debugf("NS92 Sent to Bridge: %s", LogStorageCompact(result))
				}
			}

			// Convert ETagResult to ANNTItem
			var anntData map[string]interface{}
			if result.ANNTData != nil {
				if err := json.Unmarshal(result.ANNTData, &anntData); err != nil {
					v.logger.Errorf("Failed to unmarshal ANNTData to ANNT: %v", err)
					continue
				}

			}

			anntItem := core.ANNTItem{
				ID:             result.ID,
				RegisterIP:     result.RegisterIP,
				TransactionSeq: result.TransactionSeq,
				ANNTData:       anntData,
				Namespace:      result.Namespace,
				Priority:       1,
				CreatedAt:      result.CreatedAt,
				ESN:            result.ESN,
			}

			if err := v.anntStore.StoreANNT(anntItem); err != nil {
				v.logger.Errorf("Failed to store NS92 ANNT: %v", err)
			} else if v.isSimMode {
				v.logger.Debugf("NS92 Stored: %s", LogStorageCompact(result))
			}
		}
	}
}

func (v *Vendor) Stop(ctx context.Context) error {
	if v.simulator != nil {
		v.simulator.Stop()
	}
	if v.ingestor == nil {
		return nil
	}
	v.logger.Info("Stopping 7-Eleven vendor services...")
	return v.ingestor.Stop(ctx)
}

// statusReporter provides periodic operational status updates every 10 minutes
func (v *Vendor) statusReporter() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	// Initial status report after 30 seconds
	time.Sleep(30 * time.Second)
	v.reportStatus()

	for range ticker.C {
		v.reportStatus()
	}
}

func (v *Vendor) reportStatus() {
	v.statusMutex.Lock()
	httpCount := atomic.LoadInt64(&v.httpRequestCount)
	lastReport := v.lastStatusReport
	v.lastStatusReport = time.Now()
	v.statusMutex.Unlock()

	uptime := time.Since(lastReport)

	// Get operational metrics
	activeESNs := len(v.esnToRegisterFile)
	mappings := v.stateMachine.GetIPESNMappings()
	activeMappings := len(mappings)

	// Count active handlers
	var activeHandlers int
	if v.ingestor != nil {
		v.ingestor.handlers.Range(func(key, value interface{}) bool {
			activeHandlers++
			return true
		})
	}

	mode := "production"
	if v.isSimMode {
		mode = "simulation"
	}

	v.logger.Infof("STATUS: %s mode | %d ESNs | %d IP mappings | %d handlers | %d HTTP reqs (%s)",
		mode, activeESNs, activeMappings, activeHandlers, httpCount, uptime.Round(time.Second))
}

// updateSimulator starts or restarts simulator with all current IP mappings
func (v *Vendor) updateSimulator() {
	// Get all current IP-ESN mappings
	allMappings := v.stateMachine.GetIPESNMappings()

	if len(allMappings) == 0 {
		return // No mappings yet
	}

	// Always restart simulator to keep it simple
	if v.simulator != nil {
		v.logger.Info("Restarting simulator with updated mappings...")
		v.simulator.Stop()
		v.simulator = nil
	}

	// Get all IPs
	allIPs := make([]string, 0, len(allMappings))
	for ip := range allMappings {
		allIPs = append(allIPs, ip)
	}

	// Start simulator with ALL IPs and their mappings
	target := fmt.Sprintf("http://localhost:%d", v.settings.ListenPort)
	realTime := os.Getenv("REAL_TIME") != "false"

	// Build register configs with terminal numbers from ESN assignments
	allRegisters := make([]RegisterConfig, 0, len(allIPs))
	for _, ip := range allIPs {
		esn := allMappings[ip]
		terminalNumber := "01" // Default fallback

		// Get terminal number from the original register config when ESN was assigned
		if assignedFile, exists := v.esnToRegisterFile[esn]; exists {
			// Extract terminal number from assigned file: register_X.jsonl -> terminal X
			if strings.HasPrefix(assignedFile, "register_") && strings.HasSuffix(assignedFile, ".jsonl") {
				fileNum := strings.TrimPrefix(assignedFile, "register_")
				fileNum = strings.TrimSuffix(fileNum, ".jsonl")
				if termNum, err := strconv.Atoi(fileNum); err == nil {
					terminalNumber = fmt.Sprintf("%02d", termNum)
				}
			}
		}

		allRegisters = append(allRegisters, RegisterConfig{
			IPAddress:      ip,
			TerminalNumber: terminalNumber,
			StoreNumber:    "38551",
		})
		v.logger.Infof("IP %s → ESN %s → terminal %s", ip, esn, terminalNumber)
	}

	// Create modulo-only simulator - each IP gets its assigned register file
	v.simulator = NewSimulatorWithMappings(v.logger, target, allIPs, "", realTime, allRegisters)

	go func() {
		v.logger.Infof("Starting simulator: %d IPs total (REAL_TIME=%t)", len(allIPs), realTime)
		if err := v.simulator.Start(); err != nil {
			v.logger.Errorf("Simulator error: %v", err)
		}
	}()
}
