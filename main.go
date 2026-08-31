package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

var version = "dev"

func main() {
	var (
		logLevel     = flag.Int("log", int(LogLevelInfo), "Log level (0=NONE 1=ERROR 2=WARN 3=INFO 4=DEBUG)")
		redisServer  = flag.String("redis_server", "127.0.0.1", "Redis server address")
		redisPort    = flag.Int("redis_port", 6379, "Redis server port")
		canDevice    = flag.String("can_device", "can0", "CAN device name")
		odometerFile = flag.String("odometer-file", defaultOdometerFile, "Durable odometer cache file")
		gearRatios   = flag.String("gear_ratios", "", "Bosch ECU gear ratios (comma-separated values 1-3, each 1-255, e.g. '100,150,200')")
		printVer     = flag.Bool("version", false, "Print version and exit")
	)
	flag.Parse()

	if *printVer {
		fmt.Printf("ecu-service %s\n", version)
		os.Exit(0)
	}

	if *logLevel < 0 || *logLevel > 4 {
		fmt.Fprintf(os.Stderr, "invalid log level %d\n", *logLevel)
		os.Exit(1)
	}

	var gearRatioValues []uint8
	if *gearRatios != "" {
		parts := strings.Split(*gearRatios, ",")
		if len(parts) > 3 {
			fmt.Fprintf(os.Stderr, "invalid gear_ratios: maximum 3 gears allowed, got %d\n", len(parts))
			os.Exit(1)
		}
		gearRatioValues = make([]uint8, 0, len(parts))
		for i, part := range parts {
			part = strings.TrimSpace(part)
			val, err := strconv.ParseUint(part, 10, 8)
			if err != nil {
				fmt.Fprintf(os.Stderr, "invalid gear_ratios: gear %d has invalid value '%s'\n", i+1, part)
				os.Exit(1)
			}
			if val == 0 || val > 255 {
				fmt.Fprintf(os.Stderr, "invalid gear_ratios: gear %d value must be 1-255, got %d\n", i+1, val)
				os.Exit(1)
			}
			gearRatioValues = append(gearRatioValues, uint8(val))
		}
	}

	opts := Options{
		LogLevel:        LogLevel(*logLevel),
		RedisServer:     *redisServer,
		RedisPort:       *redisPort,
		CANDevice:       *canDevice,
		OdometerFile:    *odometerFile,
		GearRatioValues: gearRatioValues,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	app, err := NewApp(ctx, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "startup error: %v\n", err)
		os.Exit(1)
	}

	if err := app.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "runtime error: %v\n", err)
		os.Exit(1)
	}
}
