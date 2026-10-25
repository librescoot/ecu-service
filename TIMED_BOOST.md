# Timed boost telemetry v1

`engine-ecu:boost` is an additive Redis string key and pub/sub channel for a TUNU native timed-boost guard. It does not change the stock boost request or acknowledgement interface.

The decoder requires the TUNU application identity (base 0x10, app version 2 or later) for the current ECU power generation, followed by an eight-byte `0x7E3` frame with extension marker `0xB1`. Generic apps without the native guard and controllers with the ordinary status format do not provide this telemetry. Known identity is cleared on each commanded power transition.

## Payload

```json
{
  "version": 1,
  "valid": true,
  "observed_at": "2026-10-25T12:00:00.123456Z",
  "state": "active",
  "reason": "none",
  "enabled": true,
  "active": true,
  "heartbeat_fresh": true,
  "supply_fresh": true,
  "eligible_packs": 2,
  "remaining_ms": 10000,
  "cooldown_ms": 0
}
```

- `observed_at` is the CAN kernel receive timestamp in UTC, not Redis publication time.
- `state` is `disabled`, `armed`, `active`, `waiting-release`, `cooldown` or `inhibited`.
- `reason` is `none`, `heartbeat-expired`, `supply-unavailable`, `motor-inhibited`, `no-headroom` or `limit-write-failed`.
- `enabled` is the guard's accepted request. It may remain true during cooldown.
- `active` means the guard is granting extra battery-current allowance. It does not mean the motor is drawing that current.
- `heartbeat_fresh` and `supply_fresh` report the independent native leases.
- `eligible_packs` is the count in the guard's leased pack mask, not a fresh measurement of every battery's physical state.
- `remaining_ms` and `cooldown_ms` are rounded-up native counters with 100 ms resolution. The wire format bounds them at 20 seconds and one hour respectively.

Malformed extension frames, unsupported identities, power-off and expired observations produce:

```json
{"version":1,"valid":false}
```

An invalid envelope has no state or timing fields. Missing/invalid/expired data means no fresh timed-boost evidence, not an authoritative inactive state.

## Freshness

Each valid observation is stored with an absolute Redis expiration of `observed_at + 500 ms`, then published with the same payload in one Redis transaction. Invalid envelopes expire after 500 ms. Reusing an unchanged observation cannot extend its expiration. When the process stops publishing, the key expires without relying on a later cleanup callback.

Consumers must check version, `valid`, and observation age. Reject future timestamps and observations at least 500 ms old, including delayed pub/sub messages. The key's expiration alone is not a reason to accept an already-buffered notification. A live native status stream can validly report an expired Lisp/supply lease; consumers needing an operational Lisp adapter must additionally require `heartbeat_fresh`.

The transport's kernel receive timestamp establishes observation age. It is not an acquisition-time guarantee for every value inside the controller frame.

## Compatibility

`settings[engine-ecu.boost]` still supplies the desired stock `0x4E0` bit 1. The `engine-ecu` hash's `boost` and `boost-status` fields retain the controller's paired-bit enable acknowledgement. Those legacy fields do not represent timed allowance activity or remaining budget. Only a compatible native guard supplies the additional fields above. The telemetry does not grant ECU CAN access to battery-service or introduce another stock control sender.
