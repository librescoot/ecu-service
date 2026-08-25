package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
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
	lastRegenAvailable bool
	lastRegenReason    string
	lastECUConfig      ECUConfigStatus
	// lastUnknownFault dedupes the unknown-code warning. The ECU repeats its
	// fault code on every status frame, so logging unconditionally would bury
	// the journal at the frame rate.
	lastUnknownFault uint32

	// frameCounts tallies received IDs for the periodic summary. Written from the
	// CAN read goroutine and read by the summary goroutine, hence the mutex.
	frameMu     sync.Mutex
	frameCounts map[uint32]int
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

	a.ecu = newECU(bus, log, opts.GearRatioValues)
	a.battery = &BatteryTracker{}
	a.ipcTx = newIPCTx(ctx, client, log)

	// Publish a zero-valued config so any stale config:* fields left behind
	// by a previous service instance get HDEL'd. Real values reappear as
	// soon as the ECU broadcasts them.
	if err := a.ipcTx.SendECUConfig(ECUConfigStatus{}); err != nil {
		log.Error("Failed to send default ECU config: %v", err)
	}

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
	// Version in the banner, not just behind --version: a log package from the
	// field is the only artifact we get, and without this line it cannot be tied
	// to a build without guessing from image tags.
	a.log.Info("ecu-service %s starting", version)

	// Subscribe to Redis channels and sync initial state.
	a.ipcRx.Start()

	// Drive the CAN bus with automatic reconnection (the SocketCAN socket goes
	// stale across MDB suspend/resume, so ConnectAndPublish returns and we must
	// rebuild the bus).
	go a.runCANBusLoop(ctx)

	// Watch for ECU comm loss (raises E20).
	go a.commLost.Run(ctx)

	// Summarise what the controller is actually sending.
	go a.runFrameSummary(ctx)

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

// frameSummaryInterval is long enough that the line is a periodic reference
// point rather than chatter, and short enough to localise a change to within a
// minute when reading a log after the fact.
const frameSummaryInterval = 60 * time.Second

// runFrameSummary logs which IDs the controller sent and how many, once per
// interval. Controllers differ in what they report and at what rate, and a
// per-ID count distinguishes a full report from a subset and gives the rate
// directly, neither of which can be recovered from the individual handlers.
//
// A period with no frames logs nothing: silence is already covered by the
// frame-gap line and the at-rest line, and a stationary locked vehicle would
// otherwise emit an empty summary every minute for as long as it sits there.
func (a *App) runFrameSummary(ctx context.Context) {
	ticker := time.NewTicker(frameSummaryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.logFrameSummary()
		}
	}
}

func (a *App) logFrameSummary() {
	a.frameMu.Lock()
	counts := a.frameCounts
	a.frameCounts = make(map[uint32]int, len(counts))
	a.frameMu.Unlock()

	if len(counts) == 0 {
		return
	}
	ids := make([]uint32, 0, len(counts))
	total := 0
	for id, n := range counts {
		ids = append(ids, id)
		total += n
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	var b strings.Builder
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%03X x%d", id, counts[id])
	}
	a.log.Info("CAN %ds: %d frames from %d IDs: %s",
		int(frameSummaryInterval.Seconds()), total, len(ids), b.String())
}

func (h *appHandler) Handle(frame can.Frame) {
	a := (*App)(h)
	a.log.DebugCAN("RX", frame.ID, frame.Data, frame.Length)
	a.ecu.HandleFrame(frame)
	a.frameMu.Lock()
	if a.frameCounts == nil {
		a.frameCounts = make(map[uint32]int)
	}
	a.frameCounts[frame.ID]++
	a.frameMu.Unlock()

	a.onFrame()
}

func (a *App) regenState(s Status) RegenState {
	// Predict availability from the KERS policy we commanded, then apply the
	// ECU's known speed and voltage gates. Status4 is an event-driven snapshot,
	// so retaining it in s.KersActive is useful for diagnostics and
	// reconciliation but it must not gate the live prediction.
	regen := computeRegen(a.ecu.KersPolicyEnabled(), a.lastKersReason, int(s.RPM), s.Voltage, s.AcceptedRegenVoltage, s.AcceptedRegenCurrent)
	return applyObservedRegen(regen, s.Current)
}

func (a *App) onFrame() {
	ratios := a.ecu.GearRatios()
	sw := a.ecu.SoftwareVersion()

	s := Status{
		Voltage:              a.ecu.Voltage(),
		Current:              a.ecu.Current(),
		RPM:                  a.ecu.RPM(),
		Speed:                a.ecu.Speed(),
		RawSpeed:             a.ecu.RawSpeed(),
		ThrottleOn:           a.ecu.ThrottleOn(),
		BrakeOn:              a.ecu.BrakeOn(),
		Power:                a.ecu.Power(),
		EnergyConsumed:       a.ecu.EnergyConsumed(),
		EnergyRecovered:      a.ecu.EnergyRecovered(),
		Temperature:          a.ecu.Temperature(),
		FaultCode:            a.ecu.FaultCode(),
		Odometer:             a.ecu.Odometer(),
		KersActive:           a.ecu.KersECUEnabled(), // publish ECU-reported KERS state (matches v1)
		BoostEnabled:         a.ecu.BoostEnabled(),
		KersReasonOff:        string(a.lastKersReason),
		AcceptedRegenVoltage: a.ecu.AcceptedRegenVoltage(),
		AcceptedRegenCurrent: a.ecu.AcceptedRegenCurrent(),
		Gear:                 a.ecu.Gear(),
		FirmwareVersion:      a.ecu.FirmwareVersion(),
		WarrantyDate:         a.ecu.WarrantyDate(),
		ECUStatusEnabled:     a.ecu.ECUStatusEnabled(),
		BoostActive:          a.ecu.BoostActive(),
		GearModeEnabled:      a.ecu.GearModeEnabled(),
		HighGearCurrent:      ratios.HighCurrent,
		MidGearCurrent:       ratios.MidCurrent,
		LowGearCurrent:       ratios.LowCurrent,
		HighGearTorque:       ratios.HighTorque,
		MidGearTorque:        ratios.MidTorque,
		LowGearTorque:        ratios.LowTorque,
		MotorRatedPowerKW:    sw.MotorRatedPowerKW,
		MotorMaxSpeedKMH:     sw.MotorMaxSpeedKMH,
		SWBaseVersion:        sw.BaseVersion,
		SWAppVersion:         sw.AppVersion,
	}
	if s.FaultCode != 0 {
		_, cfg := MapFault(s.FaultCode)
		s.FaultDesc = cfg.Description
		// A code with no entry in the table is a gap we want to hear about. This
		// is the only place it becomes visible, so log it once per distinct code
		// rather than letting it pass as an anonymous number in Redis.
		if cfg.Unknown && s.FaultCode != a.lastUnknownFault {
			a.lastUnknownFault = s.FaultCode
			a.log.Warn("ECU reported fault code %d (0x%02X), which is not in the fault table", s.FaultCode, s.FaultCode)
		}
	}
	if s.FaultCode == 0 {
		a.lastUnknownFault = 0
	}

	regen := a.regenState(s)
	s.RegenAvailable = regen.Available
	s.RegenReason = regen.Reason
	s.RegenExpected = regen.ExpectedMA

	if err := a.ipcTx.SendStatus(s); err != nil {
		a.log.Error("SendStatus: %v", err)
	}

	cfg := a.ecu.ConfigReport()
	ecuConfig := ECUConfigStatus{
		OverVoltageThresholdMV:  cfg.OverVoltageThresholdMV,
		UnderVoltageThresholdMV: cfg.UnderVoltageThresholdMV,
		SpeedLimitRatio:         cfg.SpeedLimitRatio,
		WheelCircumferenceCM:    cfg.WheelCircumferenceCM,
		MaxPhaseCurrentMA:       cfg.MaxPhaseCurrentMA,
		StartupPhaseCurrentMA:   cfg.StartupPhaseCurrentMA,
	}
	if ecuConfig != a.lastECUConfig {
		if err := a.ipcTx.SendECUConfig(ecuConfig); err != nil {
			a.log.Error("SendECUConfig: %v", err)
		} else {
			a.lastECUConfig = ecuConfig
		}
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
	if s.RegenAvailable != a.lastRegenAvailable || s.RegenReason != a.lastRegenReason {
		a.lastRegenAvailable = s.RegenAvailable
		a.lastRegenReason = s.RegenReason
		if err := a.ipcTx.PublishRegen(); err != nil {
			a.log.Error("PublishRegen: %v", err)
		}
	}

	a.diag.Update(s.FaultCode)

	// KERS is only changed while stopped; reconcile if the ECU re-enabled it
	// despite a non-none reason.
	a.kers.UpdateVehicleStopped(s.Speed == 0)
	a.kers.UpdateECUKers(a.ecu.KersECUEnabled())
}
