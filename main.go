package main

import (
    "flag"
    "fmt"
    "log"
    "os"
    "os/signal"
    "syscall"
    "ecu-service/ecu"
)

var (
    version       = flag.Bool("version", false, "Print version info")
    help          = flag.Bool("help", false, "Print help")
    logLevel      = flag.Int("log", 3, "Log level (0=NONE, 1=ERROR, 2=WARN, 3=INFO, 4=DEBUG)")
    redisServer   = flag.String("redis_server", "127.0.0.1", "Redis server address")
    redisPort     = flag.Int("redis_port", 6379, "Redis server port")
    canDevice     = flag.String("can_device", "can0", "CAN device name")
    ecuType       = flag.String("ecu_type", "bosch", "ECU type (bosch or votol)")
)

const (
    ProjectName    = "vehicle-service"
    ProjectVersion = "1.0.0"
)

func printVersion() {
    fmt.Printf("%s v%s\n", ProjectName, ProjectVersion)
}

func printHelp() {
    printVersion()
    flag.PrintDefaults()
}

func main() {
    flag.Parse()

    if *version {
        printVersion()
        os.Exit(0)
    }

    if *help {
        printHelp()
        os.Exit(0)
    }

    // Validate log level
    if *logLevel < 0 || *logLevel > 4 {
        log.Fatalf("invalid log level %d", *logLevel)
    }

    // Parse ECU type
var ecuTypeEnum ecu.ECUType
switch *ecuType {
case "bosch":
    ecuTypeEnum = ecu.ECUTypeBosch
    log.Printf("Selected ECU type: Bosch")
case "votol":
    ecuTypeEnum = ecu.ECUTypeVotol
    log.Printf("Selected ECU type: Votol")
default:
    log.Fatalf("invalid ECU type: %s (must be 'bosch' or 'votol')", *ecuType)
}

    opts := &Options{
        LogLevel:         LogLevel(*logLevel),
        RedisServerAddr:  *redisServer,
        RedisServerPort:  uint16(*redisPort),
        CANDevice:        *canDevice,
        ECUType:         ecuTypeEnum,
    }

    app, err := NewEngineApp(opts)
    if err != nil {
        log.Fatalf("failed to create engine app: %v", err)
    }
    defer app.Destroy()

    // Handle SIGINT and SIGTERM
    sigChan := make(chan os.Signal, 1)
    signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

    // Run until signal received
    <-sigChan
}
