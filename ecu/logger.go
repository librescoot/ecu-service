package ecu

// Logger interface for ECU logging
type Logger interface {
	Printf(format string, v ...interface{})
	Debug(format string, v ...interface{})
	Info(format string, v ...interface{})
	Warn(format string, v ...interface{})
	Error(format string, v ...interface{})
	DebugCAN(direction string, id uint32, data []byte, length uint8)
}

// StdLogger wraps a standard logger to implement the Logger interface
type StdLogger struct {
	logger interface {
		Printf(format string, v ...interface{})
	}
}

func NewStdLogger(logger interface{ Printf(format string, v ...interface{}) }) *StdLogger {
	return &StdLogger{logger: logger}
}

func (l *StdLogger) Printf(format string, v ...interface{}) {
	l.logger.Printf(format, v...)
}

func (l *StdLogger) Debug(format string, v ...interface{}) {
	// No-op for standard logger
}

func (l *StdLogger) Info(format string, v ...interface{}) {
	l.logger.Printf(format, v...)
}

func (l *StdLogger) Warn(format string, v ...interface{}) {
	l.logger.Printf(format, v...)
}

func (l *StdLogger) Error(format string, v ...interface{}) {
	l.logger.Printf(format, v...)
}

func (l *StdLogger) DebugCAN(direction string, id uint32, data []byte, length uint8) {
	// No-op for standard logger
}

// LogCAN logs CAN frame if logger supports DebugCAN
func LogCAN(logger Logger, direction string, id uint32, data []byte, length uint8) {
	if logger != nil {
		logger.DebugCAN(direction, id, data, length)
	}
}

// DebugCANFrame formats and logs a CAN frame
func DebugCANFrame(logger Logger, direction string, id uint32, data [8]byte, length uint8) {
	if logger != nil {
		logger.DebugCAN(direction, id, data[:], length)
	}
}
