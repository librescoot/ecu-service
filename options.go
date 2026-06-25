package main

type LogLevel int

const (
	LogLevelNone  LogLevel = 0
	LogLevelError LogLevel = 1
	LogLevelWarn  LogLevel = 2
	LogLevelInfo  LogLevel = 3
	LogLevelDebug LogLevel = 4
)

type Options struct {
	LogLevel    LogLevel
	RedisServer string
	RedisPort   int
	CANDevice   string
}
