package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"ecu-service/ecu" // Local ECU package

	"github.com/brutella/can"
	"github.com/go-redis/redis/v8"
)

const (
	EngineAppIPCRetryTime = 2 * time.Second
	EngineAppIPCRetries   = 3
)

type EngineApp struct {
	log     *log.Logger
	redis   *redis.Client
	ipcRx   *IPCRx
	ipcTx   *IPCTx
	battery *Battery
	ecu     ecu.ECUInterface // New ECU interface
	diag    *Diag
	kers    *KERS
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
}

func NewEngineApp(opts *Options) (*EngineApp, error) {
	ctx, cancel := context.WithCancel(context.Background())

	app := &EngineApp{
		log:    log.New(log.Writer(), fmt.Sprintf("[%s] ", ProjectName), log.LstdFlags),
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

	app.log.Printf("Connecting to Redis at %s:%d...", opts.RedisServerAddr, opts.RedisServerPort)

	if err := app.redis.Ping(connectCtx).Err(); err != nil {
		app.log.Printf("Failed to connect to Redis: %v", err)
		return nil, fmt.Errorf("failed to connect to Redis: %v", err)
	}
	app.log.Printf("Successfully connected to Redis")

	// Start health check goroutine
	go app.redisHealthCheck()

	// Initialize components
	app.battery = NewBattery(app.log)
	app.log.Printf("Battery component initialized")

	app.ipcTx = NewIPCTx(app.log, app.redis)
	app.log.Printf("IPC TX component initialized")

	app.kers = NewKERS(app.log, ctx, app.ipcTx)
	app.log.Printf("KERS component initialized")

	app.diag = NewDiag(app.log, app.redis)
	app.log.Printf("Diagnostics component initialized")

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
		ECUType:   ecu.ECUTypeBosch, // Default to Bosch
	}

	app.ecu = ecu.NewECU(opts.ECUType)
	if app.ecu == nil {
		return nil, fmt.Errorf("failed to create ECU of type %v", opts.ECUType)
	}

	if err := app.ecu.Initialize(ctx, ecuConfig); err != nil {
		return nil, fmt.Errorf("failed to initialize ECU: %v", err)
	}
	app.log.Printf("ECU component initialized - selected ECU type: %v", opts.ECUType)

	app.kers.SetKersEnabledCallback(func(enabled bool) error {
		return app.ecu.SetKersEnabled(enabled)
	})

	// Create frame handler for CAN messages
	handler := &frameHandler{app: app}
	bus.Subscribe(handler)

	// Start CAN message publishing
	go func() {
		if err := bus.ConnectAndPublish(); err != nil {
			app.log.Printf("CAN bus publish error: %v", err)
		}
	}()

	app.ipcRx = NewIPCRx(app.log, app.redis, app.battery, app.kers)
	if app.ipcRx == nil {
		return nil, fmt.Errorf("failed to initialize IPC RX")
	}
	app.log.Printf("IPC RX component initialized")

	return app, nil
}

// Frame handler for CAN messages
type frameHandler struct {
	app *EngineApp
}

func (h *frameHandler) Handle(frame can.Frame) {
	if err := h.app.ecu.HandleFrame(frame); err != nil {
		h.app.log.Printf("Error handling CAN frame: %v", err)
		return
	}

	// Update Redis with latest ECU state
	h.app.updateRedisState()
}

// Update Redis with current ECU state
func (app *EngineApp) updateRedisState() {
	// Get current state from ECU
	status1 := RedisStatus1{
		MotorVoltage: app.ecu.GetVoltage(),
		MotorCurrent: app.ecu.GetCurrent(),
		RPM:          app.ecu.GetRPM(),
		Speed:        app.ecu.GetSpeed(), // Direct assignment, as GetSpeed() already returns uint16
		ThrottleOn:   app.ecu.GetThrottleOn(),
	}

	status2 := RedisStatus2{
		Temperature: int(app.ecu.GetTemperature()),
	}

	status3 := RedisStatus3{
		Odometer: app.ecu.GetOdometer(),
	}

	status4 := RedisStatus4{
		KersOn: app.ecu.GetKersEnabled(),
	}

	// Send updates through IPC
	if err := app.ipcTx.SendStatus1(status1); err != nil {
		app.log.Printf("Failed to send Status1: %v", err)
	}

	if err := app.ipcTx.SendStatus2(status2); err != nil {
		app.log.Printf("Failed to send Status2: %v", err)
	}

	if err := app.ipcTx.SendStatus3(status3); err != nil {
		app.log.Printf("Failed to send Status3: %v", err)
	}

	if err := app.ipcTx.SendStatus4(status4); err != nil {
		app.log.Printf("Failed to send Status4: %v", err)
	}

	// Update diagnostics if there's a fault
	if faultCode := app.ecu.GetFaultCode(); faultCode != 0 {
		app.diag.SetEngineFaultPresence(DiagFault(faultCode))
	}
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
				app.log.Printf("Redis health check failed: %v", err)
			}
			cancel()
		}
	}
}

func (app *EngineApp) Destroy() {
	app.mu.Lock()
	defer app.mu.Unlock()

	app.log.Printf("Shutting down engine application...")

	if app.cancel != nil {
		app.cancel()
	}

	if app.ipcRx != nil {
		app.ipcRx.Destroy()
		app.log.Printf("IPC RX shutdown complete")
	}

	if app.kers != nil {
		app.kers.Destroy()
		app.log.Printf("KERS shutdown complete")
	}

	if app.battery != nil {
		app.battery.Destroy()
		app.log.Printf("Battery shutdown complete")
	}

	if app.ecu != nil {
		app.ecu.Cleanup()
		app.log.Printf("ECU shutdown complete")
	}

	if app.diag != nil {
		app.diag.Destroy()
		app.log.Printf("Diagnostics shutdown complete")
	}

	if app.ipcTx != nil {
		app.ipcTx.Destroy()
		app.log.Printf("IPC TX shutdown complete")
	}

	if app.redis != nil {
		if err := app.redis.Close(); err != nil {
			app.log.Printf("Error closing Redis connection: %v", err)
		} else {
			app.log.Printf("Redis connection closed")
		}
	}

	app.log.Printf("Engine application shutdown complete")
}
