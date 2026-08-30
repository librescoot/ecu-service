# Librescoot ECU Service

Part of the [Librescoot](https://librescoot.org/) open-source platform.

`ecu-service` bridges a supported Bosch motor controller from SocketCAN to the
Librescoot Redis IPC surface. It is intended to run on the vehicle, alongside
Redis and the CAN interface connected to the controller. On unu vehicles, the
Bosch ECU is a licensed, white-labeled Lingbo LBMC controller with CAN support.

## Capabilities

- Decodes ECU status, configuration, gear, and diagnostic frames.
- Publishes motor telemetry, controller-reported configuration, and fault state.
- Applies boost and regenerative-braking (KERS) settings from Redis.
- Gates KERS on vehicle readiness, vehicle speed, and the temperature/state of
  active batteries.
- Reconnects its CAN socket after it becomes stale, such as across suspend and
  resume.

## Operation and Redis interface

The service reads the Bosch ECU from the configured CAN interface and maintains
the `engine-ecu` hash. Its fields include motor voltage/current, speed, RPM,
power, energy counters, temperature, odometer, brake and throttle states,
controller KERS/boost state, gear information, firmware details, and decoded
configuration when the ECU reports it.

The `engine-ecu` pub/sub channel carries field names for selected events,
including `throttle`, `odometer`, `kers`, `kers-reason-off`,
`regen-available`, and `fault`. Consumers should read the hash after a
notification rather than treating the notification as a value payload.

Fault presence is stored in `engine-ecu:fault`; changes are also appended to
`events:faults` with group `engine-ecu`. The service watches `vehicle.state`,
`battery:0`, and `battery:1` to determine KERS eligibility. Battery state is
considered active only when its `state` field is `active`.

### Controller power-on behavior

The service does not immediately assert KERS and boost state when ECU power is
commanded on. It waits for the controller's initial boot period, then retries
that assertion from the watchdog until an incoming CAN frame confirms the
controller is listening. This prevents unacknowledged control frames from
being retransmitted while the controller is still booting. State changes made
while ECU power is off are not queued; the current commanded state is asserted
when the controller becomes reachable.

## Configuration

Runtime configuration is provided by command-line flags. Run
`bin/ecu-service -help` after building for the authoritative flag list. The
main deployment choices are the Redis address and port, the CAN interface
(default `can0`), log level, and optional Bosch gear ratios.

The following fields in the Redis `settings` hash are watched at startup and on
change:

- `engine-ecu.boost`
- `engine-ecu.kers`
- `engine-ecu.kers-power`
- `engine-ecu.kers-power-dual`
- `engine-ecu.kers-voltage`

KERS treats `disabled` (and the legacy value `false`) as off; an unset value is
enabled. Current and voltage values are passed to the controller, so restrict
write access to these settings to trusted vehicle-management components.

## Build and test

The Makefile uses Go 1.25.7 for its build and test targets.

```bash
make build        # Linux ARMv7 binary: bin/ecu-service
make build-host   # local-development binary: bin/ecu-service
make test
make lint         # requires golangci-lint
```

## Deployment and operations

The Yocto layer ships `librescoot-ecu.service`, which requires Valkey and
starts after the vehicle and battery services. The runtime requires a reachable
Redis-compatible datastore and permission to open the configured SocketCAN
interface.

The service handles `SIGINT` and `SIGTERM`. Loss of ECU communication while the
controller is powered is reported as the synthetic `E20` fault; investigate the
CAN path and controller power state before clearing or acting on that report.

## License

This project is licensed under the [Creative Commons Attribution-NonCommercial-ShareAlike 4.0 International License](LICENSE).

Made with ❤️ by the Librescoot community
