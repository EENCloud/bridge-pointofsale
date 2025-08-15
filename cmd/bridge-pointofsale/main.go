package main

import (
	"bridge-pointofsale/internal/api"
	"bridge-pointofsale/internal/core"
	"bridge-pointofsale/internal/settings"
	"bridge-pointofsale/internal/vendors"
	_ "bridge-pointofsale/internal/vendors/seveneleven"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	goeen_log "github.com/eencloud/goeen/log"
)

func main() {
	logger := goeen_log.NewContext(os.Stdout, "", goeen_log.LevelInfo).GetLogger("bridge-pointofsale", goeen_log.LevelInfo)
	logger.Info("Starting Bridge Devices POS application...")

	dataDir := core.GetDataDirectory()
	dbDir := filepath.Join(dataDir, "badger_db")
	anntStore, err := core.NewANNTStore(dbDir, 2, logger)
	if err != nil {
		logger.Fatalf("Failed to create ANNT store: %v", err)
	}
	defer func() {
		if err := anntStore.Close(); err != nil {
			logger.Errorf("Failed to close ANNT store: %v", err)
		}
	}()

	settingsManager := settings.NewManager(logger)

	var active vendors.Vendor
	settingsManager.SetUpdateCallback(func(esn string, vendor *settings.VendorConfig) {
		if active != nil {
			active.HandleESNConfiguration(esn, vendor)
		}
	})

	vendorGetter := func() api.Vendor {
		if active != nil {
			return active
		}
		return nil
	}

	apiAddr := ":33480"
	if port := os.Getenv("POS_SERVICE_PORT"); port != "" {
		apiAddr = ":" + port
	}

	server := api.NewServer(apiAddr, logger, settingsManager, anntStore, vendorGetter)

	go func() {
		if err := server.Start(); err != nil && err != http.ErrServerClosed {
			logger.Errorf("API Server failed: %v", err)
		}
	}()

	go func() {
		for range settingsManager.Changes() {
			cfg := settingsManager.GetActiveVendor()
			if cfg != nil && active == nil {
				newFunc, err := vendors.Get(cfg.Vendor)
				if err != nil {
					logger.Errorf("Failed to get vendor: %v", err)
					continue
				}
				raw, err := json.Marshal(cfg)
				if err != nil {
					logger.Errorf("Failed to marshal vendor config: %v", err)
					continue
				}
				v, err := newFunc(logger, raw, anntStore)
				if err != nil {
					logger.Errorf("Failed to create vendor: %v", err)
					continue
				}
				if err := v.Start(); err != nil {
					logger.Errorf("Failed to start vendor: %v", err)
					continue
				}
				active = v
				for esn, vcfg := range settingsManager.GetAllESNConfigurations() {
					active.HandleESNConfiguration(esn, vcfg)
				}
			}
			if cfg == nil && active != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				if err := active.Stop(ctx); err != nil {
					logger.Errorf("Failed to stop vendor: %v", err)
				}
				cancel()
				logger.Info("Bridge Devices POS application stopped")
				active = nil
			}
		}
	}()

	stopCh := make(chan os.Signal, 1)
	signal.Notify(stopCh, syscall.SIGINT, syscall.SIGTERM)
	<-stopCh

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	if err := server.Stop(ctx); err != nil {
		logger.Errorf("API Server stop failed: %v", err)
	}
	if active != nil {
		if err := active.Stop(ctx); err != nil {
			logger.Errorf("Vendor stop failed: %v", err)
		}
	}
	cancel()
	logger.Info("Bridge Devices POS application stopped")
}
