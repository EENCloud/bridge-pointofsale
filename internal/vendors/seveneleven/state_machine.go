package seveneleven

/*
Seven-Eleven POS State Machine with Simplified Timeout Strategy:

TIMEOUT RULES:
1. General transactions: 1-minute timeout → ABANDONED (generous and kind)
2. CCEOT (cartChangeTrail endOfTransaction): 10-second timer → Good transaction
   - If CCEOT seen, start 10-second timer
   - If no other events within 10 seconds → send NS91 as good transaction
   - If more events come after CCEOT → expect CMD for normal close (otherwise rule 1 catches it)

ABANDONMENT TRIGGERS:
- 1-minute timeout without proper ending → ABANDONED
- CCEOT + 10 seconds of silence → Good transaction (no status)
- CMD:EndTransaction or CMD:StartTransaction → Proper completion/interruption handling
*/

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/eencloud/goeen/log"
	"github.com/google/uuid"
)

// Forward declaration for processor interface
type ANNTProcessor interface {
	CreateANNTStructure(payload map[string]interface{}, state *TransactionState, namespace int) ([]byte, error)
}

// POS namespace UUID for deterministic UUIDv5 generation

type TransactionStatus string

const (
	StatusUnknown   TransactionStatus = "UNKNOWN"
	StatusAbandoned TransactionStatus = "ABANDONED"
)

type ETagResult struct {
	ID             string
	RegisterIP     string
	TransactionSeq string
	ANNTData       []byte // JSON ANNT structure
	Namespace      int
	Status         *TransactionStatus
	CreatedAt      time.Time
	ESN            string
}

type POSTransactionState struct {
	TransactionSeq              string
	RegisterIP                  string
	StartedAt                   time.Time
	LastActivity                time.Time
	IsComplete                  bool
	NS91Sent                    bool
	HasStartTriad               bool
	IndividualNS92Allowed       bool       // True after happy start triad confirmed
	CCEOTTimestamp              *time.Time // When cartChangeTrail endOfTransaction was seen
	CMD                         map[string]interface{}
	MetaData                    map[string]interface{}
	TransactionHeader           map[string]interface{}
	EndCMD                      string
	OtherEvents                 []map[string]interface{} // ALL other event types (generic) - includes transactionFooter
	PendingEventsForAggregation []map[string]interface{} // Events waiting for happy start triad
}

type UnknownEventGroup struct {
	RegisterIP   string
	Events       []map[string]interface{}
	FirstSeen    time.Time
	LastActivity time.Time
	WindowKey    string
}

type IPState struct {
	RegisterIP              string
	ActiveTransactions      map[string]*POSTransactionState
	UnknownEventGroups      map[string]*UnknownEventGroup
	PendingStartTransaction *map[string]interface{}
	LastActivity            time.Time
	mutex                   sync.RWMutex
}

type IPBasedStateMachine struct {
	ipStates map[string]*IPState
	mutex    sync.RWMutex

	ns91Queue     chan<- ETagResult
	ns92Queue     chan<- ETagResult
	processor     ANNTProcessor
	esn           string
	ipToESN       map[string]string
	staticMapping map[string]string // Static mappings that take precedence
	logger        *log.Logger
}

func NewIPBasedStateMachine(ns91Queue, ns92Queue chan<- ETagResult, logger *log.Logger) *IPBasedStateMachine {
	return &IPBasedStateMachine{
		ipStates:  make(map[string]*IPState),
		ns91Queue: ns91Queue,
		ns92Queue: ns92Queue,
		esn:       DefaultESN,
		ipToESN:   make(map[string]string),
		logger:    logger,
	}
}

func (sm *IPBasedStateMachine) SetProcessor(processor ANNTProcessor) {
	sm.processor = processor
}

// SetESN sets the ESN for ETag generation
func (sm *IPBasedStateMachine) SetESN(esn string) {
	sm.esn = esn
}

func (sm *IPBasedStateMachine) SetIPESNMappings(mappings map[string]string) {
	// In normal mode, just set the mappings directly
	if sm.ipToESN == nil {
		sm.ipToESN = make(map[string]string)
	}

	// Clear existing mappings and set new ones
	sm.ipToESN = make(map[string]string)
	for ip, esn := range mappings {
		sm.ipToESN[ip] = esn
	}
}

// SetIPESNMappingsReplaceStatic replaces all mappings including static ones (for real configs in sim mode)
func (sm *IPBasedStateMachine) SetIPESNMappingsReplaceStatic(mappings map[string]string) {
	if sm.ipToESN == nil {
		sm.ipToESN = make(map[string]string)
	}

	// Clear static mappings when real configs arrive
	sm.staticMapping = nil

	// Replace all mappings with new ones
	sm.ipToESN = make(map[string]string)
	for ip, esn := range mappings {
		sm.ipToESN[ip] = esn
	}

	sm.logger.Info("Replaced static mappings with real config mappings")
}

// SetStaticIPESNMappings sets static mappings that take precedence over dynamic ones
func (sm *IPBasedStateMachine) SetStaticIPESNMappings(mappings map[string]string) {
	sm.staticMapping = make(map[string]string)
	for ip, esn := range mappings {
		sm.staticMapping[ip] = esn
	}

	// Update the active mappings to include static ones
	if sm.ipToESN == nil {
		sm.ipToESN = make(map[string]string)
	}
	for ip, esn := range mappings {
		sm.ipToESN[ip] = esn
	}
}

func (sm *IPBasedStateMachine) AddIPESNMapping(ip, esn string) {
	if sm.ipToESN == nil {
		sm.ipToESN = make(map[string]string)
	}

	// Dynamic configs can override static mappings (simulation baseline behavior)
	sm.ipToESN[ip] = esn
	sm.logger.Infof("Added ESN %s for IP %s", esn, ip)
}

// ClearStaticMappings removes static baseline mappings (called when first real config arrives)
func (sm *IPBasedStateMachine) ClearStaticMappings() {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	if sm.staticMapping != nil {
		staticCount := len(sm.staticMapping)

		// Remove static mappings from active mappings
		for ip := range sm.staticMapping {
			delete(sm.ipToESN, ip)
		}

		// Clear static mapping reference
		sm.staticMapping = nil

		sm.logger.Infof("Cleared %d static baseline mappings - now using only real configs", staticCount)
	}
}

// GetIPESNMappings returns a copy of the current IP-to-ESN mappings
func (sm *IPBasedStateMachine) GetIPESNMappings() map[string]string {
	sm.mutex.RLock()
	defer sm.mutex.RUnlock()

	if sm.ipToESN == nil {
		return make(map[string]string)
	}

	// Return a copy to prevent external modification
	mappings := make(map[string]string)
	for ip, esn := range sm.ipToESN {
		mappings[ip] = esn
	}
	return mappings
}

func (sm *IPBasedStateMachine) getESNForIP(ip string) string {
	if sm.ipToESN != nil {
		if esn, exists := sm.ipToESN[ip]; exists {
			if isValidESN(esn) {
				return esn
			}
			sm.logger.Errorf("Invalid ESN format for IP %s: %s", ip, esn)
		}
	}

	if sm.esn != "" && isValidESN(sm.esn) {
		return sm.esn
	}

	sm.logger.Errorf("No valid ESN found for IP %s", ip)
	return InvalidESN
}

func isValidESN(esn string) bool {
	if len(esn) != 8 {
		return false
	}
	for _, c := range esn {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (sm *IPBasedStateMachine) ProcessEvent(registerIP string, eventData []byte) error {
	var data map[string]interface{}
	if err := json.Unmarshal(eventData, &data); err != nil {
		return err
	}

	ipState := sm.getOrCreateIPState(registerIP)
	ipState.mutex.Lock()
	defer ipState.mutex.Unlock()

	// Clean up old unknown events and abandoned transactions
	sm.cleanupOldUnknownEvents(ipState)
	sm.cleanupAbandonedTransactions(ipState)

	now := time.Now()
	ipState.LastActivity = now

	// RULE: Handle demarcators FIRST before checking transaction sequences
	// Demarcators (Start/End) have special handling and should never be treated as unknown events
	if sm.isStartDemarcator(data) {
		return sm.handleStartTransaction(ipState, data, now)
	}
	if sm.isEndDemarcator(data) {
		return sm.handleEndTransaction(ipState, data, now)
	}

	// Extract transaction sequence - RULE: No txnSeq = check for active transaction first
	transactionSeq := extractTransactionSeq(data)

	if transactionSeq == "" {
		// RULE: Events without transaction sequence numbers should be linked to active transaction if one exists
		// Only treat as unknown event if no active transaction exists for this IP
		if len(ipState.ActiveTransactions) == 1 {
			// Link to the single active transaction
			for seq := range ipState.ActiveTransactions {
				return sm.handleTransactionEvent(ipState, seq, data, now)
			}
		} else if len(ipState.ActiveTransactions) > 1 {
			// Multiple active transactions - link to most recent
			var latestSeq string
			var latestTime time.Time
			for seq, tx := range ipState.ActiveTransactions {
				if latestSeq == "" || tx.LastActivity.After(latestTime) {
					latestSeq = seq
					latestTime = tx.LastActivity
				}
			}
			return sm.handleTransactionEvent(ipState, latestSeq, data, now)
		}
		// No active transactions - treat as unknown event with UNKNOWN status
		return sm.handleUnknownEvent(ipState, data, now)
	}

	// RULE: Events with transaction sequence numbers → linked to transactions
	// Status determination happens when we have enough context (metadata)
	return sm.handleTransactionEvent(ipState, transactionSeq, data, now)
}

func (sm *IPBasedStateMachine) handleStartTransaction(ipState *IPState, eventData map[string]interface{}, now time.Time) error {
	// RULE: When we see a premature next start, we wait until metadata to do ABANDONED or RETRYING on aggregate
	// BUT: We can immediately clean up transactions based on their current state

	for transactionSeq, transaction := range ipState.ActiveTransactions {
		if transaction.HasStartTriad {
			// Transaction with complete start triad → will be ABANDONED when new metadata arrives
			// Send aggregate now with ABANDONED status since this pattern indicates incomplete retry
			status := StatusAbandoned
			sm.sendToNS91(transaction, &status)
			delete(ipState.ActiveTransactions, transactionSeq)
		} else {
			// Incomplete transaction (no happy start triad) → ABANDONED immediately
			// No need to wait for metadata since this transaction never got complete context
			status := StatusAbandoned
			sm.sendToNS91(transaction, &status)
			delete(ipState.ActiveTransactions, transactionSeq)
		}
	}

	// RULE: Pending StartTransaction that never got metadata → DROP (don't send as NS92)
	// StartTransaction without metadata should never be sent individually
	if ipState.PendingStartTransaction != nil {
		sm.logger.Debugf("Dropping incomplete pending StartTransaction for %s (no metadata received)", ipState.RegisterIP)
		ipState.PendingStartTransaction = nil
	}

	// Create new pending start transaction - will be linked when metaData arrives
	ipState.PendingStartTransaction = &map[string]interface{}{
		"CMD":       "StartTransaction",
		"timestamp": now,
	}

	return nil
}

func (sm *IPBasedStateMachine) handleEndTransaction(ipState *IPState, eventData map[string]interface{}, now time.Time) error {
	// RULE: EndTransaction is a sequence demarcator
	// - If it has transaction context (linked to active transaction) → complete/abandon transaction
	// - If it has no transaction context → would be handled as unknown event (UNKNOWN) in ProcessEvent
	// This function only gets called for demarcators that are CMD events without txnSeq

	// Find the most recent active transaction to potentially complete
	var latestTransaction *POSTransactionState
	var latestSeq string

	for seq, transaction := range ipState.ActiveTransactions {
		if latestTransaction == nil || transaction.LastActivity.After(latestTransaction.LastActivity) {
			latestTransaction = transaction
			latestSeq = seq
		}
	}

	// RULE: If no transaction will complete, flush unknowns now; otherwise defer unknown flush to avoid back-to-back NS91s
	if latestTransaction == nil {
		for windowKey, group := range ipState.UnknownEventGroups {
			sm.flushUnknownEventGroup(group)
			delete(ipState.UnknownEventGroups, windowKey)
		}
	}

	// Mark ALL other transactions (not the latest) as ABANDONED
	for seq, transaction := range ipState.ActiveTransactions {
		if seq != latestSeq && !transaction.IsComplete {
			status := StatusAbandoned
			sm.sendToNS91(transaction, &status)
			delete(ipState.ActiveTransactions, seq)
		}
	}

	// RULE: Pending StartTransaction that never got metadata → DROP (don't send as NS92)
	// StartTransaction without metadata should never be sent individually
	if ipState.PendingStartTransaction != nil {
		sm.logger.Debugf("Dropping incomplete pending StartTransaction for %s (no metadata received)", ipState.RegisterIP)
		ipState.PendingStartTransaction = nil
	}

	// Try to complete the latest transaction if it exists
	if latestTransaction != nil && !latestTransaction.NS91Sent {
		// Only send NS91 if happy start triad was confirmed (complete transaction)
		if latestTransaction.IndividualNS92Allowed {
			latestTransaction.IsComplete = true
			if cmd, ok := eventData["CMD"].(string); ok {
				latestTransaction.EndCMD = cmd
			}
			sm.sendToNS91(latestTransaction, nil)
		} else {
			// Incomplete transaction - send aggregate NS91 with ABANDONED status
			status := StatusAbandoned
			sm.sendToNS91(latestTransaction, &status)
		}
		latestTransaction.NS91Sent = true
		delete(ipState.ActiveTransactions, latestSeq)
	}
	// RULE: If no active transactions exist, this EndTransaction is just a demarcator
	// Demarcators are NEVER sent as individual NS92s - they only trigger flushing actions
	// The unknown events were already flushed above

	return nil
}

func (sm *IPBasedStateMachine) getOrCreateIPState(registerIP string) *IPState {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	if state, exists := sm.ipStates[registerIP]; exists {
		return state
	}

	state := &IPState{
		RegisterIP:              registerIP,
		ActiveTransactions:      make(map[string]*POSTransactionState),
		UnknownEventGroups:      make(map[string]*UnknownEventGroup),
		PendingStartTransaction: nil,
		LastActivity:            time.Now(),
	}
	sm.ipStates[registerIP] = state
	return state
}

func (sm *IPBasedStateMachine) handleTransactionEvent(ipState *IPState, transactionSeq string, eventData map[string]interface{}, now time.Time) error {
	// RULE: Events with transaction sequence numbers get linked to transactions
	// Status determination (RETRYING vs ABANDONED) happens when we have metadata context

	transaction, exists := ipState.ActiveTransactions[transactionSeq]
	if !exists {
		transaction = &POSTransactionState{
			TransactionSeq:              transactionSeq,
			RegisterIP:                  ipState.RegisterIP,
			StartedAt:                   now,
			LastActivity:                now,
			OtherEvents:                 make([]map[string]interface{}, 0),
			PendingEventsForAggregation: make([]map[string]interface{}, 0),
		}

		// Link pending StartTransaction if available
		if ipState.PendingStartTransaction != nil {
			transaction.CMD = *ipState.PendingStartTransaction
			ipState.PendingStartTransaction = nil
		}

		ipState.ActiveTransactions[transactionSeq] = transaction
	}

	transaction.LastActivity = now

	// Reset CCEOT timer if new events come after CCEOT (expect CMD for normal close)
	if transaction.CCEOTTimestamp != nil {
		transaction.CCEOTTimestamp = nil
		sm.logger.Infof("Transaction %s: New event after CCEOT - expecting CMD for normal close", transaction.TransactionSeq)
	}

	// RULE: Handle metadata events - this is where we determine duplicate vs new transactions
	if _, ok := eventData["metaData"]; ok {
		// Check for duplicate: if transaction with same sequence already has metadata
		if transaction.MetaData != nil {
			// Same txnSeq seen again → ABANDONED status (register retry failed)
			status := StatusAbandoned
			sm.sendToNS91(transaction, &status)

			// Clear the old transaction and start fresh with same sequence
			delete(ipState.ActiveTransactions, transactionSeq)
			transaction = &POSTransactionState{
				TransactionSeq: transactionSeq,
				RegisterIP:     ipState.RegisterIP,
				StartedAt:      now,
				LastActivity:   now,
				OtherEvents:    make([]map[string]interface{}, 0),
			}

			// Link pending StartTransaction if available
			if ipState.PendingStartTransaction != nil {
				transaction.CMD = *ipState.PendingStartTransaction
				ipState.PendingStartTransaction = nil
			}

			ipState.ActiveTransactions[transactionSeq] = transaction
		} else {
			// Check for ABANDONED: if there are other incomplete transactions on this IP
			for otherSeq, otherTx := range ipState.ActiveTransactions {
				if otherSeq != transactionSeq && otherTx.MetaData != nil && !otherTx.IsComplete {
					// Mark the other transaction as ABANDONED (full aggregate to NS91)
					status := StatusAbandoned
					sm.sendToNS91(otherTx, &status)
					delete(ipState.ActiveTransactions, otherSeq)
				}
			}
		}
		transaction.MetaData = eventData
	} else if _, ok := eventData["transactionHeader"]; ok {
		transaction.TransactionHeader = eventData
	} else {
		// Everything else goes into OtherEvents - completely generic for ANY vendor event type
		// This includes transactionFooter, cartChangeTrail, paymentSummary, and ANY unknown types
		transaction.OtherEvents = append(transaction.OtherEvents, eventData)

		// Check for cartChangeTrail endOfTransaction - start 10-second timer
		if cartChangeTrail, ok := eventData["cartChangeTrail"].(map[string]interface{}); ok {
			if eventResult, exists := cartChangeTrail["eventResult"].(string); exists {
				if eventResult == "endOfTransaction" {
					now := time.Now()
					transaction.CCEOTTimestamp = &now
					sm.logger.Infof("Transaction %s: CCEOT detected, starting 10-second timer", transaction.TransactionSeq)
				}
			}
		}
	}

	// Check for happy start triad completion
	if transaction.CMD != nil && transaction.MetaData != nil && transaction.TransactionHeader != nil && !transaction.HasStartTriad {
		transaction.HasStartTriad = true
		transaction.IndividualNS92Allowed = true // Now allow individual NS92s

		// Send optional Type 1A NS92 (combined start sequence)
		sm.sendCombinedStartTriadNS92(transaction)

		// Process any pending events that were waiting for happy start triad
		for _, pendingEvent := range transaction.PendingEventsForAggregation {
			sm.sendType1BNS92(transaction, pendingEvent)
		}
		transaction.PendingEventsForAggregation = nil // Clear pending events
	}

	// Individual events after happy start triad confirmed
	if transaction.IndividualNS92Allowed {
		skipIndividualNS92 := false
		if sm.isStartDemarcator(eventData) || sm.isEndDemarcator(eventData) {
			skipIndividualNS92 = true
		} else if _, ok := eventData["metaData"]; ok {
			skipIndividualNS92 = true
		} else if _, ok := eventData["transactionHeader"]; ok {
			skipIndividualNS92 = true
		} else if _, ok := eventData["paymentSummary"]; ok {
			skipIndividualNS92 = true
		} else if _, ok := eventData["transactionSummary"]; ok {
			skipIndividualNS92 = true
		} else if _, ok := eventData["transactionFooter"]; ok {
			skipIndividualNS92 = true
		}

		if !skipIndividualNS92 {
			sm.sendType1BNS92(transaction, eventData)
		}
	} else {
		// Happy start triad not yet confirmed - add to pending events for later aggregation
		// BUT skip start triad components (CMD, metaData, transactionHeader)
		if _, isCmd := eventData["CMD"]; !isCmd {
			if _, isMetaData := eventData["metaData"]; !isMetaData {
				if _, isHeader := eventData["transactionHeader"]; !isHeader {
					if transaction.PendingEventsForAggregation == nil {
						transaction.PendingEventsForAggregation = make([]map[string]interface{}, 0)
					}
					transaction.PendingEventsForAggregation = append(transaction.PendingEventsForAggregation, eventData)
				}
			}
		}
	}

	// Check for transaction completion (close demarcators)
	if sm.isEndDemarcator(eventData) {
		transaction.IsComplete = true
		if cmd, ok := eventData["CMD"].(string); ok {
			transaction.EndCMD = cmd
		} else {
			transaction.EndCMD = ""
		}
		if transaction.IndividualNS92Allowed {
			sm.sendToNS91(transaction, nil)
		} else {
			status := StatusAbandoned
			sm.sendToNS91(transaction, &status)
		}
		delete(ipState.ActiveTransactions, transactionSeq)
	}

	return nil
}

func (sm *IPBasedStateMachine) handleUnknownEvent(ipState *IPState, eventData map[string]interface{}, now time.Time) error {

	// RULE: Events without transaction sequence numbers → UNKNOWN status
	// These include: age verification failures, system errors, random events between transactions

	windowKey := fmt.Sprintf("%d", now.Unix()/30)

	group, exists := ipState.UnknownEventGroups[windowKey]
	if !exists {
		group = &UnknownEventGroup{
			RegisterIP:   ipState.RegisterIP,
			Events:       make([]map[string]interface{}, 0),
			FirstSeen:    now,
			LastActivity: now,
			WindowKey:    windowKey,
		}
		ipState.UnknownEventGroups[windowKey] = group
	}

	group.Events = append(group.Events, eventData)
	group.LastActivity = now

	// RULE: Unknown events accumulate and are sent as NS91 with UNKNOWN status when demarcator hits
	// NO individual NS92s for unknown events - they are waste

	if sm.isUnknownEventGroupComplete(group) {
		sm.flushUnknownEventGroup(group)
		delete(ipState.UnknownEventGroups, windowKey)
	}

	return nil
}

func (sm *IPBasedStateMachine) cleanupOldUnknownEvents(ipState *IPState) {
	cutoff := time.Now().Add(-30 * time.Second)
	for windowKey, group := range ipState.UnknownEventGroups {
		if group.LastActivity.Before(cutoff) {
			sm.flushUnknownEventGroup(group)
			delete(ipState.UnknownEventGroups, windowKey)
		}
	}
}

func (sm *IPBasedStateMachine) cleanupAbandonedTransactions(ipState *IPState) {
	now := time.Now()

	// 30 second general timeout - faster cleanup
	generalTimeout := now.Add(-30 * time.Second)

	// 10 seconds after CCEOT - quick completion for likely finished transactions
	ccEndTimeout := now.Add(-10 * time.Second)

	for transactionSeq, transaction := range ipState.ActiveTransactions {
		if !transaction.IsComplete {
			shouldTimeout := false
			var status *TransactionStatus

			// Check CCEOT 10-second rule first (higher priority)
			if transaction.CCEOTTimestamp != nil && transaction.CCEOTTimestamp.Before(ccEndTimeout) {
				shouldTimeout = true
				status = nil // Good transaction - no status
				sm.logger.Infof("Transaction %s: CCEOT timeout - treating as completed", transactionSeq)
			} else if transaction.LastActivity.Before(generalTimeout) {
				// General 1-minute abandonment rule
				shouldTimeout = true
				s := StatusAbandoned
				status = &s
				sm.logger.Infof("Transaction %s: General timeout - marking as ABANDONED", transactionSeq)
			}

			if shouldTimeout {
				sm.sendToNS91(transaction, status)
				delete(ipState.ActiveTransactions, transactionSeq)
			}
		}
	}
}

// unified demarcator helpers
func (sm *IPBasedStateMachine) isStartDemarcator(eventData map[string]interface{}) bool {
	if cmd, ok := eventData["CMD"].(string); ok {
		return cmd == "StartTransaction"
	}
	return false
}

func (sm *IPBasedStateMachine) isEndDemarcator(eventData map[string]interface{}) bool {
	if cmd, ok := eventData["CMD"].(string); ok {
		if cmd == "EndTransaction" {
			return true
		}
	}
	// REMOVED: cartChangeTrail eventResult "endOfTransaction" as end demarcator
	// This allows cartChangeTrail events to be individual NS92s and enables
	// smart status determination based on the last cart activity type
	return false
}

// hasSignificantTransactionContent checks if transaction has meaningful content
// beyond just start triad - used for abandonment decisions
func (sm *IPBasedStateMachine) hasSignificantTransactionContent(transaction *POSTransactionState) bool {
	if len(transaction.OtherEvents) == 0 {
		return false
	}

	// Count meaningful events (not just system/status events)
	meaningfulEvents := 0
	for _, event := range transaction.OtherEvents {
		// cartChangeTrail, paymentSummary, etc. are meaningful
		if _, hasCart := event["cartChangeTrail"]; hasCart {
			meaningfulEvents++
		} else if _, hasPayment := event["paymentSummary"]; hasPayment {
			meaningfulEvents++
		} else if _, hasFooter := event["transactionFooter"]; hasFooter {
			meaningfulEvents++
		}
		// Add other meaningful event types as needed
	}

	return meaningfulEvents > 0
}

func (sm *IPBasedStateMachine) isUnknownEventGroupComplete(group *UnknownEventGroup) bool {
	hasMetaData := false
	hasCartChange := false

	for _, event := range group.Events {
		if _, ok := event["metaData"]; ok {
			hasMetaData = true
		}
		if _, ok := event["cartChangeTrail"]; ok {
			hasCartChange = true
		}
	}

	return hasMetaData && hasCartChange
}

func (sm *IPBasedStateMachine) flushUnknownEventGroup(group *UnknownEventGroup) {
	// RULE: All unknown event groups get UNKNOWN status as NS91 - these are events without transaction context
	// NO NS92s for unknown events - they are waste!
	status := StatusUnknown

	// Create a fake transaction for unknown events to send as NS91
	fakeTransaction := &POSTransactionState{
		RegisterIP:     group.RegisterIP,
		TransactionSeq: "", // No sequence for unknown events
		OtherEvents:    group.Events,
	}

	sm.sendToNS91(fakeTransaction, &status)
}

func (sm *IPBasedStateMachine) sendToNS91(transaction *POSTransactionState, status *TransactionStatus) {
	posData := map[string]interface{}{
		"domain":      "711pos2",
		"register_ip": transaction.RegisterIP,
	}

	// Only add CMD if we have an actual end command (not for unknown event aggregates)
	if transaction.EndCMD != "" {
		posData["CMD"] = transaction.EndCMD
	}

	if transaction.MetaData != nil {
		if metaData, ok := transaction.MetaData["metaData"]; ok && metaData != nil {
			posData["metaData"] = metaData
		}
	}

	// Safely add transactionHeader if available
	if transaction.TransactionHeader != nil {
		if header, ok := transaction.TransactionHeader["transactionHeader"]; ok && header != nil {
			posData["transactionHeader"] = header
		}
	}

	// Add ALL other events generically with NS91 aggregation rules
	for _, event := range transaction.OtherEvents {
		for key, value := range event {
			if key != "metaData" && key != "transactionHeader" && key != "CMD" {
				if existingValue, exists := posData[key]; exists {
					// Multiple values - handle based on simplified rules
					if existingArray, isArray := existingValue.([]interface{}); isArray {
						// Already an array, append new value
						posData[key] = append(existingArray, value)
					} else {
						// Convert to array: objects become [{}, {}], arrays become [[], []]
						posData[key] = []interface{}{existingValue, value}
					}
				} else {
					// First occurrence - apply simplified rules
					if valueArray, isArray := value.([]interface{}); isArray {
						// Arrays always get double-wrapped [[...]] for NS91
						posData[key] = []interface{}{valueArray}
					} else {
						// Objects stay as-is {...} for NS91 when single
						posData[key] = value
					}
				}
			}
		}
	}

	// Smart status determination: EXCEPTION for cartChangeTrail endOfTransaction
	if status == nil {
		// EXCEPTION: If transaction had CCEOT, send as good transaction without status
		if transaction.CCEOTTimestamp != nil {
			// Send as good transaction - no status needed
		} else if !sm.hasSignificantTransactionContent(transaction) {
			// No meaningful content = abandoned
			s := StatusAbandoned
			status = &s
		}
		// Note: If has meaningful content but NOT CCEOT, also send without status (good transaction)
	}
	if status != nil {
		posData["status"] = string(*status)
	}

	completeTransaction := map[string]interface{}{
		"_pos": posData,
	}

	etag := ETagResult{
		ID:             sm.GenerateETagIDFromEvent(transaction.RegisterIP, transaction.TransactionSeq, transaction.MetaData),
		RegisterIP:     transaction.RegisterIP,
		TransactionSeq: transaction.TransactionSeq,
		Namespace:      91,
		Status:         status,
		CreatedAt:      time.Now(),
		ESN:            sm.getESNForIP(transaction.RegisterIP),
	}

	if sm.processor != nil {
		state := &TransactionState{
			UUID: sm.GenerateETagBinaryUUID(transaction.RegisterIP, transaction.TransactionSeq, transaction.MetaData),
			Seq:  uint16(len(transaction.OtherEvents) + 4),
			ESN:  sm.getESNForIP(transaction.RegisterIP),
		}

		if anntData, err := sm.processor.CreateANNTStructure(completeTransaction, state, 91); err == nil {
			etag.ANNTData = anntData
		}
	}

	select {
	case sm.ns91Queue <- etag:
	default:
	}
}

func (sm *IPBasedStateMachine) sendToNS92(eventData map[string]interface{}, registerIP, transactionSeq string, status *TransactionStatus) {

	etag := ETagResult{
		ID:             sm.GenerateETagIDFromEvent(registerIP, transactionSeq, eventData),
		RegisterIP:     registerIP,
		TransactionSeq: transactionSeq,
		Namespace:      92,
		Status:         status,
		CreatedAt:      time.Now(),
		ESN:            sm.getESNForIP(registerIP),
	}

	if sm.processor != nil {
		state := &TransactionState{
			UUID: sm.GenerateETagBinaryUUID(registerIP, transactionSeq, eventData),
			Seq:  1,
			ESN:  sm.getESNForIP(registerIP),
		}

		if anntData, err := sm.processor.CreateANNTStructure(eventData, state, 92); err == nil {
			etag.ANNTData = anntData
		}
	}

	select {
	case sm.ns92Queue <- etag:
	default:
	}
}

func (sm *IPBasedStateMachine) sendCombinedStartTriadNS92(transaction *POSTransactionState) {
	posData := map[string]interface{}{
		"domain":      "711pos2",
		"register_ip": transaction.RegisterIP,
		"CMD":         "StartTransaction",
	}

	// Safely add metaData if available
	if transaction.MetaData != nil {
		if metaData, ok := transaction.MetaData["metaData"]; ok && metaData != nil {
			posData["metaData"] = metaData
		}
	}

	// Safely add transactionHeader if available
	if transaction.TransactionHeader != nil {
		if header, ok := transaction.TransactionHeader["transactionHeader"]; ok && header != nil {
			posData["transactionHeader"] = header
		}
	}

	combinedEvent := map[string]interface{}{
		"_pos": posData,
	}
	sm.sendToNS92(combinedEvent, transaction.RegisterIP, transaction.TransactionSeq, nil)
}

func (sm *IPBasedStateMachine) sendType1BNS92(transaction *POSTransactionState, eventData map[string]interface{}) {
	posData := map[string]interface{}{
		"domain":      "711pos2",
		"register_ip": transaction.RegisterIP,
	}

	if transaction.MetaData != nil {
		if metaData, ok := transaction.MetaData["metaData"]; ok && metaData != nil {
			posData["metaData"] = metaData
		}
	}

	// Add the individual event data (cartChangeTrail, paymentSummary, etc.)
	for key, value := range eventData {
		if key != "metaData" { // Don't overwrite the reinserted metadata
			posData[key] = value
		}
	}

	enrichedEvent := map[string]interface{}{
		"_pos": posData,
	}

	sm.sendToNS92(enrichedEvent, transaction.RegisterIP, transaction.TransactionSeq, nil)
}

func extractTransactionSeq(eventData map[string]interface{}) string {
	if metaData, ok := eventData["metaData"].(map[string]interface{}); ok {
		if seq, exists := metaData["transactionSeqNumber"].(string); exists {
			// Trim whitespace and check if non-empty
			trimmed := strings.TrimSpace(seq)
			if trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// GenerateETagIDFromEvent creates UUID for individual events (unknown events, NS92)
func (sm *IPBasedStateMachine) GenerateETagIDFromEvent(registerIP, transactionSeq string, eventData map[string]interface{}) string {
	// Try UUIDv5 if this event has complete metaData
	if metaData, ok := eventData["metaData"].(map[string]interface{}); ok {
		if store, hasStore := metaData["storeNumber"].(string); hasStore && store != "" {
			if terminal, hasTerminal := metaData["terminalNumber"].(string); hasTerminal && terminal != "" {
				if txn, hasTxn := metaData["transactionSeqNumber"].(string); hasTxn && txn != "" {
					// UUIDv5 for events with complete transaction info
					nameString := fmt.Sprintf("%s:%s:%s:%s", registerIP, store, terminal, txn)
					transactionUUID := uuid.NewSHA1(posNamespaceUUID, []byte(nameString))
					return transactionUUID.String()
				}
			}
		}
	}

	// UUIDv4 for unknown events and incomplete events
	return uuid.New().String()
}

// GenerateETagBinaryUUID creates the UUID for the ETag header (binary field)
// This can be the same logic but returns the actual UUID for binary embedding
func (sm *IPBasedStateMachine) GenerateETagBinaryUUID(registerIP, transactionSeq string, eventData map[string]interface{}) uuid.UUID {
	// Try UUIDv5 if this event has complete metaData
	if metaData, ok := eventData["metaData"].(map[string]interface{}); ok {
		if store, hasStore := metaData["storeNumber"].(string); hasStore && store != "" {
			if terminal, hasTerminal := metaData["terminalNumber"].(string); hasTerminal && terminal != "" {
				if txn, hasTxn := metaData["transactionSeqNumber"].(string); hasTxn && txn != "" {
					// UUIDv5 for deterministic linking
					nameString := fmt.Sprintf("%s:%s:%s:%s", registerIP, store, terminal, txn)
					return uuid.NewSHA1(posNamespaceUUID, []byte(nameString))
				}
			}
		}
	}

	// UUIDv4 for unknown events
	return uuid.New()
}
