package main

import (
	"fmt"
	"log"
)

// LeveledLogger wraps a standard logger with log level filtering
type LeveledLogger struct {
	logger   *log.Logger
	logLevel LogLevel
}

// NewLeveledLogger creates a new leveled logger
func NewLeveledLogger(logger *log.Logger, level LogLevel) *LeveledLogger {
	return &LeveledLogger{
		logger:   logger,
		logLevel: level,
	}
}

// Debug logs a message at DEBUG level
func (l *LeveledLogger) Debug(format string, v ...interface{}) {
	if l.logLevel >= LogLevelDebug {
		l.logger.Printf("[DEBUG] "+format, v...)
	}
}

// Info logs a message at INFO level
func (l *LeveledLogger) Info(format string, v ...interface{}) {
	if l.logLevel >= LogLevelInfo {
		l.logger.Printf("[INFO] "+format, v...)
	}
}

// Warn logs a message at WARN level
func (l *LeveledLogger) Warn(format string, v ...interface{}) {
	if l.logLevel >= LogLevelWarn {
		l.logger.Printf("[WARN] "+format, v...)
	}
}

// Error logs a message at ERROR level
func (l *LeveledLogger) Error(format string, v ...interface{}) {
	if l.logLevel >= LogLevelError {
		l.logger.Printf("[ERROR] "+format, v...)
	}
}

// Printf provides compatibility with standard logger - logs at INFO level
func (l *LeveledLogger) Printf(format string, v ...interface{}) {
	l.Info(format, v...)
}

// Fatalf logs a fatal error and exits
func (l *LeveledLogger) Fatalf(format string, v ...interface{}) {
	l.logger.Fatalf("[FATAL] "+format, v...)
}

// SetLevel changes the log level
func (l *LeveledLogger) SetLevel(level LogLevel) {
	l.logLevel = level
}

// GetLevel returns the current log level
func (l *LeveledLogger) GetLevel() LogLevel {
	return l.logLevel
}

// DebugCAN logs CAN frame details at DEBUG level with formatting
func (l *LeveledLogger) DebugCAN(direction string, id uint32, data []byte, length uint8) {
	if l.logLevel >= LogLevelDebug {
		dataStr := ""
		for i := uint8(0); i < length && i < 8; i++ {
			dataStr += fmt.Sprintf("%02X ", data[i])
		}
		l.logger.Printf("[DEBUG] CAN %s: ID=0x%03X Len=%d Data=[%s]", direction, id, length, dataStr)
	}
}

// Ensure LeveledLogger implements ecu.Logger interface at compile time
var _ interface {
	Printf(format string, v ...interface{})
	Debug(format string, v ...interface{})
	Info(format string, v ...interface{})
	Warn(format string, v ...interface{})
	Error(format string, v ...interface{})
	DebugCAN(direction string, id uint32, data []byte, length uint8)
} = (*LeveledLogger)(nil)
