package service

import (
	"bridge-pointofsale/internal/core"
	"bridge-pointofsale/internal/settings"
	"bridge-pointofsale/internal/vendors"
	"context"
	"encoding/json"

	"github.com/eencloud/goeen/log"
)

type VendorManager struct {
	logger       *log.Logger
	anntStore    *core.ANNTStore
	activeVendor vendors.Vendor
}

func NewVendorManager(logger *log.Logger, anntStore *core.ANNTStore) *VendorManager {
	return &VendorManager{
		logger:    logger,
		anntStore: anntStore,
	}
}

func (vm *VendorManager) HandleConfigChange(vendorConfig *settings.VendorConfig) error {
	shouldRestart := vm.shouldRestartVendor(vendorConfig)

	if shouldRestart {
		if err := vm.stopCurrentVendor(); err != nil {
			return err
		}

		if vendorConfig != nil {
			return vm.startNewVendor(vendorConfig)
		}
	}

	return nil
}

func (vm *VendorManager) shouldRestartVendor(vendorConfig *settings.VendorConfig) bool {
	if vm.activeVendor == nil {
		vm.logger.Info("No active vendor - starting new vendor")
		return true
	}
	if vendorConfig == nil {
		vm.logger.Info("No vendor configuration - stopping current vendor")
		return true
	}

	vm.logger.Infof("Vendor already active - no restart needed")
	return false
}

func (vm *VendorManager) stopCurrentVendor() error {
	if vm.activeVendor != nil {
		vm.logger.Infof("Stopping current vendor: %s", vm.activeVendor.Name())
		if err := vm.activeVendor.Stop(context.Background()); err != nil {
			vm.logger.Errorf("Error stopping vendor %s: %v", vm.activeVendor.Name(), err)
			return err
		}
		vm.activeVendor = nil
	}
	return nil
}

func (vm *VendorManager) startNewVendor(vendorConfig *settings.VendorConfig) error {
	newFunc, err := vendors.Get("7eleven_registers")
	if err != nil {
		vm.logger.Errorf("Failed to get vendor: %v", err)
		return err
	}

	vendorJSON, _ := json.Marshal(vendorConfig)
	newVendor, err := newFunc(vm.logger, vendorJSON, vm.anntStore)
	if err != nil {
		vm.logger.Errorf("Failed to create vendor: %v", err)
		return err
	}

	if err := newVendor.Start(); err != nil {
		vm.logger.Errorf("Failed to start vendor: %v", err)
		return err
	}

	vm.activeVendor = newVendor
	return nil
}

func (vm *VendorManager) HandleESNConfiguration(esn string, vendor *settings.VendorConfig) {
	if vm.activeVendor != nil {
		vm.activeVendor.HandleESNConfiguration(esn, vendor)
	}
}

func (vm *VendorManager) Stop() error {
	return vm.stopCurrentVendor()
}
