package ecu

import (
    "context"
    "sync"
    "time"
    "github.com/brutella/can"
)

const (
    // Common constants
    SpeedToleranceFactor = 1.155556
    CalibrationFactor    = 1.03
    RPMToSpeedFactor     = 0.0783744
    OdoCalFactor         = 1.07

    // Window size for speed averaging
    WindowSize = 3

    // Timeout for stale ECU data (if no frames received in this time, data is considered stale)
    ECUDataTimeout = 2 * time.Second
)

// BaseECU contains common ECU functionality
type BaseECU struct {
    mu              sync.RWMutex
    logger          Logger
    bus             *can.Bus
    ctx             context.Context
    cancel          context.CancelFunc
    speedBuffer     SpeedBuffer
    lastFrameTime   time.Time  // Timestamp of last received CAN frame
}

// SpeedBuffer implements a moving average for speed readings
type SpeedBuffer struct {
    data  [WindowSize]uint16
    head  uint8
    count uint8
    sum   uint16
}

func (buf *SpeedBuffer) Reset() {
    buf.count = 0
    buf.head = 0
    buf.sum = 0
    for i := range buf.data {
        buf.data[i] = 0
    }
}

func (buf *SpeedBuffer) MovingAverage(speed uint16) float64 {
    var lastData uint16
    if buf.count >= WindowSize {
        buf.count = WindowSize
        lastData = buf.data[buf.head]
    } else {
        buf.count++
    }

    buf.data[buf.head] = speed
    buf.sum = (buf.sum - lastData) + speed
    average := float64(buf.sum) / float64(buf.count)
    buf.head = (buf.head + 1) % WindowSize

    return average
}

// InitializeBase initializes the base ECU functionality
func (b *BaseECU) InitializeBase(ctx context.Context, config ECUConfig) error {
    b.mu.Lock()
    defer b.mu.Unlock()

    b.logger = config.Logger
    b.bus = config.CANBus
    b.ctx, b.cancel = context.WithCancel(ctx)
    b.lastFrameTime = time.Now()

    return nil
}

// CleanupBase performs cleanup of base ECU resources
func (b *BaseECU) CleanupBase() {
    if b.cancel != nil {
        b.cancel()
    }
}

// UpdateFrameTimestamp updates the timestamp of the last received frame
// Should be called by ECU implementations when processing frames
func (b *BaseECU) UpdateFrameTimestamp() {
    b.lastFrameTime = time.Now()
}

// IsDataStale returns true if no frames have been received within the timeout period
func (b *BaseECU) IsDataStale() bool {
    return time.Since(b.lastFrameTime) > ECUDataTimeout
}

// calculateSpeed processes raw speed input using calibration and averaging
func (b *BaseECU) calculateSpeed(rawSpeed uint16) uint16 {
    if rawSpeed == 0 {
        b.speedBuffer.Reset()
        return 0
    }

    avgSpeed := b.speedBuffer.MovingAverage(rawSpeed)
    return uint16(avgSpeed * CalibrationFactor * SpeedToleranceFactor)
}

// packFrame creates a CAN frame with the given ID and data
func packFrame(id uint32, data []byte) can.Frame {
    var frameData [8]byte
    copy(frameData[:], data)
    return can.Frame{
        ID:     id,
        Length: uint8(len(data)),
        Flags:  0,
        Data:   frameData,
    }
}

// isStatusMessage checks if a CAN ID represents a status message
func isStatusMessage(id uint32) bool {
    return (id & 0xF00) == 0x700
}

// Helper function to convert bool to byte
func boolToByte(b bool) byte {
    if b {
        return 1
    }
    return 0
}
