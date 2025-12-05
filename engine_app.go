package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"ecu-service/ecu" // Local ECU package

	"github.com/brutella/can"
	"github.com/go-redis/redis/v8"
)

const (
	EngineAppIPCRetryTime = 2 * time.Second
	EngineAppIPCRetries   = 3

	// Fault recovery timing constants
	// After a fault is detected, wait before requesting ECU status update
	FaultUpdateDelay = 500 * time.Millisecond
	// If fault persists this long without clearing, force clear it
	FaultClearTimeout = 5 * time.Second
)

type EngineApp struct {
	log       *LeveledLogger
	redis     *redis.Client
	ipcRx     *IPCRx
	ipcTx     *IPCTx
	battery   *Battery
	ecu       ecu.ECUInterface
	diag      *Diag
	kers      *KERS
	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	lastSpeed uint16 // Track last sent speed

	// Fault recovery timers
	faultUpdateTimer *time.Timer // Timer to request ECU status after fault
	faultClearTimer  *time.Timer // Timer to force-clear stuck faults
	hasFault         bool        // Track if we currently have an active fault
}

// writeDefaultRedisState writes default values to Redis
func (app *EngineApp) writeDefaultRedisState() {
	app.mu.Lock()
	defer app.mu.Unlock()

	// Default Status1 values
	status1 := RedisStatus1{
		MotorVoltage: 0,     // 0V
		MotorCurrent: 0,     // 0A
		RPM:          0,     // 0 RPM
		Speed:        0,     // 0 km/h
		ThrottleOn:   false, // Throttle off
	}

	// Default Status2 values
	status2 := RedisStatus2{
		Temperature: 0, // 0°C
	}

	// Default Status3 values
	status3 := RedisStatus3{
		Odometer: 0, // 0 meters
	}

	// Default Status4 values
	status4 := RedisStatus4{
		KersOn:  false, // KERS disabled
		BoostOn: false, // Boost disabled
	}

	// Write all default values to Redis
	if err := app.ipcTx.SendStatus1(status1); err != nil {
		app.log.Error("Failed to send default Status1: %v", err)
	}

	if err := app.ipcTx.SendStatus2(status2); err != nil {
		app.log.Error("Failed to send default Status2: %v", err)
	}

	if err := app.ipcTx.SendStatus3(status3); err != nil {
		app.log.Error("Failed to send default Status3: %v", err)
	}

	if err := app.ipcTx.SendStatus4(status4); err != nil {
		app.log.Error("Failed to send default Status4: %v", err)
	}

	app.log.Debug("Default Redis state written")
}

func NewEngineApp(opts *Options) (*EngineApp, error) {
	ctx, cancel := context.WithCancel(context.Background())

	app := &EngineApp{
		log:    opts.Logger,
		ctx:    ctx,
		cancel: cancel,
	}

	// Initialize Redis client with timeouts
	app.redis = redis.NewClient(&redis.Options{
		Addr:         fmt.Sprintf("%s:%d", opts.RedisServerAddr, opts.RedisServerPort),
		Password:     "",
		DB:           0,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	})

	// Test Redis connection with timeout
	connectCtx, connectCancel := context.WithTimeout(ctx, 5*time.Second)
	defer connectCancel()

	app.log.Info("Connecting to Redis at %s:%d...", opts.RedisServerAddr, opts.RedisServerPort)

	if err := app.redis.Ping(connectCtx).Err(); err != nil {
		app.log.Error("Failed to connect to Redis: %v", err)
		return nil, fmt.Errorf("failed to connect to Redis: %v", err)
	}
	app.log.Info("Connected to Redis")

	// Initialize components
	app.battery = NewBattery(app.log)
	app.log.Debug("Battery component initialized")

	app.ipcTx = NewIPCTx(app.log, app.redis)
	app.log.Debug("IPC TX component initialized")

	// Write default values to Redis after ipcTx is initialized
	app.writeDefaultRedisState()

	// Start health check goroutines
	go app.redisHealthCheck()
	// Note: ecuStaleDataCheck removed - ECU pauses CAN during flash writes which triggered false positives

	app.kers = NewKERS(app.log, ctx, app.ipcTx)
	app.log.Debug("KERS component initialized")

	app.diag = NewDiag(app.log, app.redis)
	app.log.Debug("Diagnostics component initialized")

	// Initialize CAN bus
	bus, err := can.NewBusForInterfaceWithName(opts.CANDevice)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize CAN bus: %v", err)
	}

	// Create and initialize ECU
	ecuConfig := ecu.ECUConfig{
		Logger:    app.log,
		CANDevice: opts.CANDevice,
		CANBus:    bus,
		ECUType:   opts.ECUType,
	}

	app.ecu = ecu.NewECU(opts.ECUType)
	if app.ecu == nil {
		return nil, fmt.Errorf("failed to create ECU of type %v", opts.ECUType)
	}

	if err := app.ecu.Initialize(ctx, ecuConfig); err != nil {
		return nil, fmt.Errorf("failed to initialize ECU: %v", err)
	}
	app.log.Info("ECU initialized: %v", opts.ECUType)

	app.kers.SetKersEnabledCallback(func(enabled bool) error {
		return app.ecu.SetKersEnabled(enabled)
	})

	// Create frame handler for CAN messages
	handler := &frameHandler{app: app}
	bus.Subscribe(handler)

	// Start CAN message publishing
	go func() {
		if err := bus.ConnectAndPublish(); err != nil {
			app.log.Error("CAN bus publish error: %v", err)
		}
	}()

	app.ipcRx = NewIPCRx(app.log, app.redis, app.battery, app.kers)
	if app.ipcRx == nil {
		return nil, fmt.Errorf("failed to initialize IPC RX")
	}
	app.log.Debug("IPC RX component initialized")

	// Set boost callback to forward settings changes to ECU
	app.ipcRx.SetBoostCallback(func(enabled bool) error {
		return app.ecu.SetBoostEnabled(enabled)
	})

	return app, nil
}

// Frame handler for CAN messages
type frameHandler struct {
	app *EngineApp
}

func (h *frameHandler) Handle(frame can.Frame) {
	// Log incoming CAN frame at DEBUG level
	h.app.log.DebugCAN("RX", frame.ID, frame.Data[:], frame.Length)

	if err := h.app.ecu.HandleFrame(frame); err != nil {
		h.app.log.Error("Error handling CAN frame: %v", err)
		return
	}

	// Update Redis with latest ECU state
	h.app.updateRedisState()
}

// Update Redis with current ECU state
func (app *EngineApp) updateRedisState() {
	app.mu.Lock()
	defer app.mu.Unlock()

	// Get current state from ECU
	currentSpeed := app.ecu.GetSpeed()
	rawSpeed := app.ecu.GetRawSpeed()

	// Only update if speed has changed
	if currentSpeed != app.lastSpeed {
		status1 := RedisStatus1{
			MotorVoltage: app.ecu.GetVoltage(),
			MotorCurrent: app.ecu.GetCurrent(),
			RPM:          app.ecu.GetRPM(),
			Speed:        currentSpeed,
			RawSpeed:     rawSpeed,
			ThrottleOn:   app.ecu.GetThrottleOn(),
		}

		if err := app.ipcTx.SendStatus1(status1); err != nil {
			app.log.Error("Failed to send Status1: %v", err)
		} else {
			app.lastSpeed = currentSpeed
		}
	}

	// Always update other statuses as they might have changed
	faultCode := app.ecu.GetFaultCode()
	faultDesc := ""
	if faultCode != 0 {
		// Get description for the fault code
		activeFaults := app.ecu.GetActiveFaults()
		for fault := range activeFaults {
			if config, ok := ecu.GetFaultConfig(fault); ok {
				faultDesc = config.Description
				break
			}
		}
	}

	status2 := RedisStatus2{
		Temperature:      int(app.ecu.GetTemperature()),
		FaultCode:        faultCode,
		FaultDescription: faultDesc,
	}

	status3 := RedisStatus3{
		Odometer: app.ecu.GetOdometer(),
	}

	status4 := RedisStatus4{
		KersOn:  app.ecu.GetKersEnabled(),
		BoostOn: app.ecu.GetBoostEnabled(),
	}

	if err := app.ipcTx.SendStatus2(status2); err != nil {
		app.log.Error("Failed to send Status2: %v", err)
	}

	if err := app.ipcTx.SendStatus3(status3); err != nil {
		app.log.Error("Failed to send Status3: %v", err)
	}

	if err := app.ipcTx.SendStatus4(status4); err != nil {
		app.log.Error("Failed to send Status4: %v", err)
	}

	status5 := RedisStatus5{
		FirmwareVersion: app.ecu.GetFirmwareVersion(),
		Gear:            app.ecu.GetGear(),
	}

	if err := app.ipcTx.SendStatus5(status5); err != nil {
		app.log.Error("Failed to send Status5: %v", err)
	}

	activeFaults := app.ecu.GetActiveFaults()
	app.diag.SetFaults(activeFaults)

	// Handle fault state changes and recovery timers
	app.handleFaultState(activeFaults)
}

func (app *EngineApp) redisHealthCheck() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-app.ctx.Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(app.ctx, 2*time.Second)
			if err := app.redis.Ping(ctx).Err(); err != nil {
				app.log.Warn("Redis health check failed: %v", err)
			}
			cancel()
		}
	}
}

// handleFaultState manages fault recovery timers based on current fault state
// Must be called with app.mu held
func (app *EngineApp) handleFaultState(activeFaults map[ecu.ECUFault]bool) {
	hasFault := len(activeFaults) > 0

	if hasFault && !app.hasFault {
		// Fault just appeared - start recovery timers
		app.log.Info("Fault detected, starting recovery timers")
		app.startFaultRecoveryTimers()
	} else if !hasFault && app.hasFault {
		// Fault just cleared - stop recovery timers
		app.log.Info("Fault cleared, stopping recovery timers")
		app.stopFaultRecoveryTimers()
	} else if hasFault {
		// Fault still present - refresh the update timer (but not the clear timer)
		app.refreshFaultUpdateTimer()
	}

	app.hasFault = hasFault
}

// startFaultRecoveryTimers initializes both fault recovery timers
func (app *EngineApp) startFaultRecoveryTimers() {
	// Stop any existing timers first
	app.stopFaultRecoveryTimers()

	// Start the update timer - requests ECU status after delay
	app.faultUpdateTimer = time.AfterFunc(FaultUpdateDelay, func() {
		app.log.Info("Fault update timer expired, requesting ECU status")
		if err := app.ecu.RequestStatusUpdate(); err != nil {
			app.log.Error("Failed to request ECU status: %v", err)
		}
	})

	// Start the clear timer - force clears faults after timeout
	app.faultClearTimer = time.AfterFunc(FaultClearTimeout, func() {
		app.log.Warn("Fault clear timer expired, forcing fault clear")
		app.mu.Lock()
		defer app.mu.Unlock()
		// Force clear all faults in diagnostics
		app.diag.SetFaults(make(map[ecu.ECUFault]bool))
		app.hasFault = false
	})
}

// refreshFaultUpdateTimer resets the update timer while fault is still present
// This ensures we request status update shortly after fault packets stop arriving
func (app *EngineApp) refreshFaultUpdateTimer() {
	if app.faultUpdateTimer != nil {
		app.faultUpdateTimer.Stop()
		app.faultUpdateTimer = time.AfterFunc(FaultUpdateDelay, func() {
			app.log.Info("Fault update timer expired, requesting ECU status")
			if err := app.ecu.RequestStatusUpdate(); err != nil {
				app.log.Error("Failed to request ECU status: %v", err)
			}
		})
	}
}

// stopFaultRecoveryTimers stops both fault recovery timers
func (app *EngineApp) stopFaultRecoveryTimers() {
	if app.faultUpdateTimer != nil {
		app.faultUpdateTimer.Stop()
		app.faultUpdateTimer = nil
	}
	if app.faultClearTimer != nil {
		app.faultClearTimer.Stop()
		app.faultClearTimer = nil
	}
}


func (app *EngineApp) Destroy() {
	app.mu.Lock()
	defer app.mu.Unlock()

	app.log.Info("Shutting down...")

	// Stop fault recovery timers
	app.stopFaultRecoveryTimers()

	if app.cancel != nil {
		app.cancel()
	}

	if app.ipcRx != nil {
		app.ipcRx.Destroy()
	}

	if app.kers != nil {
		app.kers.Destroy()
	}

	if app.battery != nil {
		app.battery.Destroy()
	}

	if app.ecu != nil {
		app.ecu.Cleanup()
	}

	if app.diag != nil {
		app.diag.Destroy()
	}

	if app.ipcTx != nil {
		app.ipcTx.Destroy()
	}

	if app.redis != nil {
		if err := app.redis.Close(); err != nil {
			app.log.Error("Error closing Redis: %v", err)
		}
	}

	app.log.Info("Shutdown complete")
}
