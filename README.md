# ecu-service

Interfaces with the Bosch ECU (motor controller) over CAN and bridges it to
Redis. Reads the ECU's status frames, publishes vehicle metrics, manages KERS
regenerative braking, and reports motor faults.

Part of the [Librescoot](https://librescoot.org/) platform. Bosch-only; there
is no Votol support.

## What it does

- Parses the Bosch status frames (`0x7E0`-`0x7E8`): speed, RPM, voltage,
  current, power, temperature, odometer, fault code, gear, firmware version.
- Publishes them to the `engine-ecu` Redis hash, writing only changed fields.
- Manages KERS: enables regen only while stopped and engine-ready, with the
  battery in an acceptable temperature band and not disabled via settings;
  defers the engine-on write ~1.5s for the ECU to initialize, and forces the
  ECU back off if it re-enables regen on a cold/hot pack.
- Raises an `E20` comm-lost fault when the ECU is powered but stops talking,
  and reconnects the CAN socket automatically across suspend/resume.

## Build

```bash
make build        # ARM (armv7), production target
make build-host   # local platform, for development
make test
```

Cross-compilation uses `GOTOOLCHAIN=go1.25.7`, `CGO_ENABLED=0`, static linking.

## Run

```bash
ecu-service [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `-log` | `3` | Log level (0=none 1=error 2=warn 3=info 4=debug) |
| `-redis_server` | `127.0.0.1` | Redis address |
| `-redis_port` | `6379` | Redis port |
| `-can_device` | `can0` | CAN interface |
| `-version` | | Print version and exit |

When run under systemd/journald, timestamps are left to the journal.

## Redis interface

Publishes the `engine-ecu` hash: `motor:voltage`, `motor:current`, `rpm`,
`speed`, `raw-speed`, `throttle`, `brake`, `power`, `energy:consumed`,
`energy:recovered`, `temperature`, `fault:code`, `fault:description`,
`odometer`, `kers`, `boost`, `kers-reason-off`, `gear`, `fw-version`,
`warranty-date`. Change notifications go out on the `engine-ecu`,
`engine-ecu throttle`, `engine-ecu odometer`, `engine-ecu kers`, and
`engine-ecu kers-reason-off` channels.

Faults are written to the `engine-ecu:fault` set and the `events:faults`
stream. The `kers` and `boost` fields reflect the state the ECU acknowledges
(Status4), not the commanded value.

Consumes vehicle state (`vehicle.state`), battery state and temperature
(`battery:0`, `battery:1`), and these settings:

- `engine-ecu.boost` - boost mode
- `engine-ecu.kers` - KERS enable/disable (default enabled)
- `engine-ecu.kers-power` - regen current (mA), single battery
- `engine-ecu.kers-power-dual` - regen current (mA) when both batteries active
- `engine-ecu.kers-voltage` - regen target voltage (mV), clamped 42000-58000

See [tech-reference](https://github.com/librescoot/unu-tech-reference) for the
full Redis and fault-code documentation.
