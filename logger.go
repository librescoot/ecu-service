package main

import (
	"fmt"
	"log"
	"os"
)

type Logger struct {
	l     *log.Logger
	level LogLevel
}

func newLogger(level LogLevel) *Logger {
	flags := log.LstdFlags | log.Lmicroseconds
	if os.Getenv("JOURNAL_STREAM") != "" {
		flags = 0
	}
	return &Logger{
		l:     log.New(os.Stdout, "", flags),
		level: level,
	}
}

func (lg *Logger) Debug(format string, v ...interface{}) {
	if lg.level >= LogLevelDebug {
		lg.l.Printf("[DEBUG] "+format, v...)
	}
}

func (lg *Logger) Info(format string, v ...interface{}) {
	if lg.level >= LogLevelInfo {
		lg.l.Printf("[INFO] "+format, v...)
	}
}

func (lg *Logger) Warn(format string, v ...interface{}) {
	if lg.level >= LogLevelWarn {
		lg.l.Printf("[WARN] "+format, v...)
	}
}

func (lg *Logger) Error(format string, v ...interface{}) {
	if lg.level >= LogLevelError {
		lg.l.Printf("[ERROR] "+format, v...)
	}
}

func (lg *Logger) Fatal(format string, v ...interface{}) {
	lg.l.Fatalf("[FATAL] "+format, v...)
}

func (lg *Logger) DebugCAN(dir string, id uint32, data [8]byte, length uint8) {
	if lg.level < LogLevelDebug {
		return
	}
	s := ""
	for i := uint8(0); i < length && i < 8; i++ {
		s += fmt.Sprintf("%02X ", data[i])
	}
	lg.l.Printf("[DEBUG] CAN %s: ID=0x%03X Len=%d Data=[%s]", dir, id, length, s)
}
