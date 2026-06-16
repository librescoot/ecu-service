package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

var version = "dev"

func main() {
	var (
		logLevel    = flag.Int("log", int(LogLevelInfo), "Log level (0=NONE 1=ERROR 2=WARN 3=INFO 4=DEBUG)")
		redisServer = flag.String("redis_server", "127.0.0.1", "Redis server address")
		redisPort   = flag.Int("redis_port", 6379, "Redis server port")
		canDevice   = flag.String("can_device", "can0", "CAN device name")
		printVer    = flag.Bool("version", false, "Print version and exit")
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

	opts := Options{
		LogLevel:    LogLevel(*logLevel),
		RedisServer: *redisServer,
		RedisPort:   *redisPort,
		CANDevice:   *canDevice,
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
