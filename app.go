package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/brutella/can"
	ipc "github.com/librescoot/redis-ipc"
)

const redisHealthInterval = 30 * time.Second

const (
	// CAN reconnect backoff after ConnectAndPublish returns (stale socket).
	canReconnectInitialBackoff = 500 * time.Millisecond
	canReconnectMaxBackoff     = 10 * time.Second
)

type App struct {
	opts     Options
	log      *Logger
	ipc      *ipc.Client
	ecu      *ECU
	battery  *BatteryTracker
	kers     *KERSController
	diag     *Diagnostics
	ipcTx    *IPCTx
	ipcRx    *IPCRx
	commLost *CommLostWatcher

	// busMu guards bus, which the CAN reconnect loop swaps out on resume.
	busMu sync.Mutex
	bus   *can.Bus

	// Change tracking for publish-on-change fields.
	lastThrottle       bool
	lastOdometer       uint32
	lastKersReason     KERSReason
	lastAppliedVoltage int
	lastAppliedCurrent int
}

func NewApp(ctx context.Context, opts Options) (*App, error) {
	log := newLogger(opts.LogLevel)

	a := &App{opts: opts, log: log}

	// Connect Redis via redis-ipc.
	client, err := ipc.New(
		ipc.WithAddress(opts.RedisServer),
		ipc.WithPort(opts.RedisPort),
		ipc.WithOnDisconnect(func(err error) {
			log.Error("Redis disconnected: %v — restarting service", err)
			panic("Redis disconnected")
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("redis connect: %w", err)
	}
	a.ipc = client
	log.Info("Connected to Redis at %s:%d", opts.RedisServer, opts.RedisPort)

	// Open CAN bus.
	bus, err := can.NewBusForInterfaceWithName(opts.CANDevice)
	if err != nil {
		return nil, fmt.Errorf("CAN bus open: %w", err)
	}
	a.bus = bus

	a.ecu = newECU(bus, log)
	a.battery = &BatteryTracker{}
	a.ipcTx = newIPCTx(ctx, client, log)

	a.lastKersReason = KERSReasonNone

	// KERS callbacks — called from within KERSController when state changes.
	a.kers = newKERSController(ctx,
		func(enabled bool) {
			a.ecu.SetKersEnabled(enabled)
			if err := a.ipcTx.PublishKERS(); err != nil {
				log.Error("PublishKERS: %v", err)
			}
		},
		func(reason KERSReason) {
			a.lastKersReason = reason
			if err := a.ipcTx.PublishKERSReasonOff(); err != nil {
				log.Error("PublishKERSReasonOff: %v", err)
			}
		},
	)

	// Diagnostics callback — called when fault is committed or cleared.
	a.diag = newDiagnostics(ctx, log, func(fault Fault, cfg FaultConfig) {
		if err := a.ipcTx.ReportFault(fault, cfg); err != nil {
			log.Error("ReportFault: %v", err)
		}
	})

	a.ipcRx = newIPCRx(client, log, a.battery, a.kers, a.ecu)
	a.commLost = newCommLostWatcher(client, a.ecu, log, a.onCommLostChange)

	return a, nil
}

// onCommLostChange publishes or clears the synthetic E20 fault. On clear it
// restores whatever fault the ECU is currently reporting (FaultNone if none).
func (a *App) onCommLostChange(raise bool) {
	if raise {
		cfg := faultConfigs[FaultECUCommLost]
		if err := a.ipcTx.SetFault(uint32(FaultECUCommLost), cfg.Description); err != nil {
			a.log.Error("SetFault E20: %v", err)
		}
		if err := a.ipcTx.ReportFault(FaultECUCommLost, cfg); err != nil {
			a.log.Error("ReportFault E20: %v", err)
		}
		return
	}
	code := a.ecu.FaultCode()
	fault, cfg := MapFault(code)
	if err := a.ipcTx.SetFault(code, cfg.Description); err != nil {
		a.log.Error("SetFault clear: %v", err)
	}
	if err := a.ipcTx.ReportFault(fault, cfg); err != nil {
		a.log.Error("ReportFault clear: %v", err)
	}
}

func (a *App) Run(ctx context.Context) error {
	a.log.Info("ecu-service starting")

	// Subscribe to Redis channels and sync initial state.
	a.ipcRx.Start()

	// Drive the CAN bus with automatic reconnection (the SocketCAN socket goes
	// stale across MDB suspend/resume, so ConnectAndPublish returns and we must
	// rebuild the bus).
	go a.runCANBusLoop(ctx)

	// Watch for ECU comm loss (raises E20).
	go a.commLost.Run(ctx)

	<-ctx.Done()
	a.log.Info("Shutting down")

	// Unblock ConnectAndPublish so the loop can observe ctx cancellation.
	a.busMu.Lock()
	if a.bus != nil {
		a.bus.Disconnect()
	}
	a.busMu.Unlock()

	a.ipc.Close()
	return nil
}

// runCANBusLoop runs ConnectAndPublish in a loop, rebuilding the bus whenever
// it returns. ConnectAndPublish blocks until the socket dies (e.g. after
// suspend/resume); on return we wait out a backoff, create a fresh bus,
// resubscribe the frame handler, and point the ECU at the new socket.
func (a *App) runCANBusLoop(ctx context.Context) {
	a.busMu.Lock()
	bus := a.bus
	a.busMu.Unlock()

	backoff := canReconnectInitialBackoff
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		bus.Subscribe((*appHandler)(a))
		a.ecu.RequestStatus()

		if err := bus.ConnectAndPublish(); err != nil {
			a.log.Error("CAN bus error: %v", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		newBus, err := can.NewBusForInterfaceWithName(a.opts.CANDevice)
		if err != nil {
			a.log.Error("Failed to recreate CAN bus on %s: %v", a.opts.CANDevice, err)
			backoff = min(backoff*2, canReconnectMaxBackoff)
			continue
		}

		a.ecu.UpdateBus(newBus)
		a.busMu.Lock()
		a.bus = newBus
		a.busMu.Unlock()
		a.log.Info("CAN bus reconnected on %s", a.opts.CANDevice)

		bus = newBus
		backoff = canReconnectInitialBackoff
	}
}

// appHandler implements can.Handler.
type appHandler App

func (h *appHandler) Handle(frame can.Frame) {
	a := (*App)(h)
	a.log.DebugCAN("RX", frame.ID, frame.Data, frame.Length)
	a.ecu.HandleFrame(frame)
	a.onFrame()
}

func (a *App) onFrame() {
	s := Status{
		Voltage:             a.ecu.Voltage(),
		Current:             a.ecu.Current(),
		RPM:                 a.ecu.RPM(),
		Speed:               a.ecu.Speed(),
		RawSpeed:            a.ecu.RawSpeed(),
		ThrottleOn:          a.ecu.ThrottleOn(),
		BrakeOn:             a.ecu.BrakeOn(),
		Power:               a.ecu.Power(),
		EnergyConsumed:      a.ecu.EnergyConsumed(),
		EnergyRecovered:     a.ecu.EnergyRecovered(),
		Temperature:         a.ecu.Temperature(),
		FaultCode:           a.ecu.FaultCode(),
		Odometer:            a.ecu.Odometer(),
		KersActive:          a.ecu.KersECUEnabled(), // publish ECU-reported KERS state (matches v1)
		BoostEnabled:        a.ecu.BoostEnabled(),
		KersReasonOff:       string(a.lastKersReason),
		AppliedRegenVoltage: a.ecu.AppliedRegenVoltage(),
		AppliedRegenCurrent: a.ecu.AppliedRegenCurrent(),
		Gear:                a.ecu.Gear(),
		FirmwareVersion:     a.ecu.FirmwareVersion(),
		WarrantyDate:        a.ecu.WarrantyDate(),
	}
	if s.FaultCode != 0 {
		_, cfg := MapFault(s.FaultCode)
		s.FaultDesc = cfg.Description
	}

	if err := a.ipcTx.SendStatus(s); err != nil {
		a.log.Error("SendStatus: %v", err)
	}

	if s.ThrottleOn != a.lastThrottle {
		a.lastThrottle = s.ThrottleOn
		if err := a.ipcTx.PublishThrottle(); err != nil {
			a.log.Error("PublishThrottle: %v", err)
		}
	}
	if s.Odometer != a.lastOdometer {
		a.lastOdometer = s.Odometer
		if err := a.ipcTx.PublishOdometer(); err != nil {
			a.log.Error("PublishOdometer: %v", err)
		}
	}
	if s.AppliedRegenVoltage != a.lastAppliedVoltage || s.AppliedRegenCurrent != a.lastAppliedCurrent {
		a.lastAppliedVoltage = s.AppliedRegenVoltage
		a.lastAppliedCurrent = s.AppliedRegenCurrent
		if err := a.ipcTx.PublishKERSApplied(); err != nil {
			a.log.Error("PublishKERSApplied: %v", err)
		}
	}

	a.diag.Update(s.FaultCode)

	// KERS is only changed while stopped; reconcile if the ECU re-enabled it
	// despite a non-none reason.
	a.kers.UpdateVehicleStopped(s.Speed == 0)
	a.kers.UpdateECUKers(a.ecu.KersECUEnabled())
}
