package main

import (
	"ecu-service/ecu"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

var version = "dev"

var (
	versionFlag = flag.Bool("version", false, "Print version info")
	help        = flag.Bool("help", false, "Print help")
	logLevel    = flag.Int("log", 3, "Log level (0=NONE, 1=ERROR, 2=WARN, 3=INFO, 4=DEBUG)")
	redisServer = flag.String("redis_server", "127.0.0.1", "Redis server address")
	redisPort   = flag.Int("redis_port", 6379, "Redis server port")
	canDevice   = flag.String("can_device", "can0", "CAN device name")
	ecuType     = flag.String("ecu_type", "bosch", "ECU type (bosch or votol)")
	gearRatios  = flag.String("gear_ratios", "", "Bosch ECU gear ratios (comma-separated values 1-3, each 1-255, e.g. '100,150,200')")
)

func printVersion() {
	fmt.Printf("ecu-service %s\n", version)
}

func printHelp() {
	printVersion()
	flag.PrintDefaults()
}

func main() {
	flag.Parse()

	if *versionFlag {
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

	// Create base logger - remove timestamp/prefix when running under systemd/journald
	var baseLogger *log.Logger
	if os.Getenv("JOURNAL_STREAM") != "" {
		baseLogger = log.New(os.Stdout, "", 0)
	} else {
		baseLogger = log.New(os.Stdout, "", log.LstdFlags)
	}

	// Create leveled logger wrapper
	logger := NewLeveledLogger(baseLogger, LogLevel(*logLevel))

	log.Printf("librescoot-ecu %s starting", version)

	// Parse ECU type
	var ecuTypeEnum ecu.ECUType
	switch *ecuType {
	case "bosch":
		ecuTypeEnum = ecu.ECUTypeBosch
		logger.Info("Selected ECU type: Bosch")
	case "votol":
		ecuTypeEnum = ecu.ECUTypeVotol
		logger.Info("Selected ECU type: Votol")
	default:
		logger.Fatalf("invalid ECU type: %s (must be 'bosch' or 'votol')", *ecuType)
	}

	// Parse gear ratios if provided
	var gearRatioValues []uint8
	if *gearRatios != "" {
		parts := strings.Split(*gearRatios, ",")
		if len(parts) > 3 {
			logger.Fatalf("invalid gear_ratios: maximum 3 gears allowed, got %d", len(parts))
		}
		gearRatioValues = make([]uint8, 0, len(parts))
		for i, part := range parts {
			part = strings.TrimSpace(part)
			val, err := strconv.ParseUint(part, 10, 8)
			if err != nil {
				logger.Fatalf("invalid gear_ratios: gear %d has invalid value '%s'", i+1, part)
			}
			if val == 0 || val > 255 {
				logger.Fatalf("invalid gear_ratios: gear %d value must be 1-255, got %d", i+1, val)
			}
			gearRatioValues = append(gearRatioValues, uint8(val))
		}
		logger.Info("Loaded %d gear ratio(s): %v", len(gearRatioValues), gearRatioValues)
	}

	opts := &Options{
		LogLevel:        LogLevel(*logLevel),
		RedisServerAddr: *redisServer,
		RedisServerPort: uint16(*redisPort),
		CANDevice:       *canDevice,
		ECUType:         ecuTypeEnum,
		Logger:          logger,
		GearRatioValues: gearRatioValues,
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
