package api

import (
	"bridge-devices-pos/internal/core"
	"bridge-devices-pos/internal/settings"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/eencloud/goeen/log"
)

// Vendor interface for accessing vendor information
type Vendor interface {
	GetIPESNMappings() map[string]string
}

// VendorGetter function type for getting the active vendor
type VendorGetter func() Vendor

// Server handles HTTP communication from bridge-core.
type Server struct {
	*http.Server
	Logger          *log.Logger
	SettingsManager *settings.Manager
	ANNTStore       *core.ANNTStore // ANNT architecture
	GetActiveVendor VendorGetter
}

// NewServer creates and configures a new server for bridge-core communication.
func NewServer(addr string, logger *log.Logger, sm *settings.Manager, anntStore *core.ANNTStore, vendorGetter VendorGetter) *Server {
	mux := http.NewServeMux()

	s := &Server{
		Server: &http.Server{
			Addr:           addr,
			Handler:        mux,
			ReadTimeout:    5 * time.Second,
			WriteTimeout:   10 * time.Second,
			MaxHeaderBytes: 1 << 20,
		},
		Logger:          logger,
		SettingsManager: sm,
		ANNTStore:       anntStore,
		GetActiveVendor: vendorGetter,
	}

	mux.HandleFunc("/pos_config", s.settingsHandler)
	mux.HandleFunc("/pos_config/current", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		esn := r.URL.Query().Get("cameraid")
		if esn == "" {
			http.Error(w, "cameraid parameter required", http.StatusBadRequest)
			return
		}
		// Return whatever the SettingsManager currently holds for this ESN
		configs := sm.GetAllESNConfigurations()
		if v, ok := configs[esn]; ok {
			_ = json.NewEncoder(w).Encode(v)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("{}"))
	})
	mux.HandleFunc("/events", s.eventsHandler)              // Bridge-core polls for events
	mux.HandleFunc("/register_data", s.registerDataHandler) // Debug endpoint for raw JSON data
	mux.HandleFunc("/config", s.configHandler)              // Debug endpoint for resolved config
	mux.HandleFunc("/metrics", s.metricsHandler)            // Bridge-core heartbeat for future application metric etags
	mux.HandleFunc("/driver", s.driverHandler)              // Serve the Lua script for discovery

	return s
}

// Start begins listening for HTTP requests.
func (s *Server) Start() error {
	s.Logger.Infof("Starting API Server on %s", s.Addr)
	return s.ListenAndServe()
}

// Stop gracefully shuts down the server.
func (s *Server) Stop(ctx context.Context) error {
	s.Logger.Info("Shutting down API Server...")
	return s.Shutdown(ctx)
}
