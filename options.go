package main

import (
    "ecu-service/ecu"
    "log"
)

type LogLevel int

const (
    LogLevelNone  LogLevel = 0
    LogLevelError LogLevel = 1
    LogLevelWarn  LogLevel = 2
    LogLevelInfo  LogLevel = 3
    LogLevelDebug LogLevel = 4
)

type Options struct {
    LogLevel         LogLevel
    RedisServerAddr  string
    RedisServerPort  uint16
    CANDevice        string
    ECUType          ecu.ECUType
    Logger           *log.Logger
}
