# Radio observation heartbeat proposal

Reference baseline: official WaveLogGate v2.1.1, commit `9344a4852ad461b60a50fbb384e2afdf17f6e414`.

## Problem statement

> Official WaveLogGate v2.1.1 does not continuously export successful unchanged radio polls. ShackCQ’s current 15-second dual-path pre-command policy therefore prevents reliable idle-radio control.

The current poller reads the configured controller every second, but publishes only a changed state or a forced update after 30 minutes. The WebSocket hub replays its retained status to a new browser with a newly generated timestamp. Those behaviours make four distinct facts look alike:

1. a successful radio observation;
2. replay of a cached status;
3. connectivity to the local gateway transport;
4. a failed radio poll.

An old exported observation does not, by itself, show that the physical radio is disconnected. It only shows that no recent successful observation has been exported.

## Proposed client protocol

Keep the one-second controller poll. Export a successful observation immediately on first read, state change, recovery after a failed poll, and at a bounded five-second cadence while the radio remains unchanged.

Every client process creates a random `session_id`. Every actual controller poll attempt increments a `sequence` within that session. Successful observations carry:

```json
{
  "type": "radio_status",
  "frequency": 14042300,
  "mode": "USB",
  "timestamp": 1790995800000,
  "session_id": "4c7701b6b7994e388c34d1c0bff455a9",
  "sequence": 42
}
```

`timestamp` is the time of the successful controller read. Socket activity, welcome frames, failed polls, and delivery attempts cannot set it.

On browser connection, the hub may replay the last observation only with `cached: true`, preserving the original timestamp, session and sequence. A replay helps render last-known state but never refreshes readiness.

Poll and server-delivery results are separate, transient evidence:

```json
{"type":"radio_poll_status","state":"poll_failed","session_id":"...","sequence":43,"timestamp":1790995801000,"failure_code":"RADIO_POLL_FAILED"}
{"type":"radio_delivery_status","state":"server_delivered","session_id":"...","sequence":44,"timestamp":1790995802000,"observed_at":1790995802000}
```

A poll failure has no `observed_at`. The first failure is emitted immediately; repeated failures are rate-limited to the observation cadence. Recovery is a new successful observation even when frequency and mode are unchanged.

## Credential-bound server path

The same successful observation is posted through the existing v2 radio endpoint with additive fields:

- `source_session_id`
- `source_sequence`
- `source_observed_at`

The server must store those values with its credential/station binding and its own `received_at`. A status read used by ShackCQ must return them without replacing `source_observed_at` with cache or request time. The client reports `server_delivered` only after a 2xx response and `server_delivery_failed` after transport or non-2xx failure.

The legacy v1 request body remains byte-shape compatible and carries none of these fields. For v2, the fields are additive. A server that has not implemented this capability may ignore them, but it cannot provide dual-path freshness; ShackCQ must remain fail-closed until the credential-bound response echoes the identity. If a strict v2 implementation rejects unknown fields, capability negotiation must precede enabling the extension for that server.

## Consumer rules

Local and server evidence is the same observation only when station/credential binding, `session_id`, `sequence`, frequency and mode match. The consumer must apply these rules:

- Accept only a successful, non-cached local observation and a credential-bound server observation with the same identity and state.
- Maintain a per-session sequence watermark. Ignore `sequence <= watermark` so delayed or reordered reports cannot refresh readiness.
- Treat a new session as a client restart. Accept it only after both paths agree on the new session and observation.
- Never advance freshness from `welcome`, socket open/close, cached replay, `poll_failed`, `server_delivered`, or `server_delivery_failed` alone.
- If local WSS delivery fails, the browser does not receive the local half and cannot satisfy the dual-path gate even if the server succeeds.
- If server delivery fails, the local half can update last-known display but cannot satisfy the dual-path gate.
- Browser reconnect first renders any cached observation as last-known. A later live unchanged observation can recover readiness when its matching server observation arrives.

These rules make missing, delayed and independently failed paths fail closed without declaring the radio physically disconnected.

## Cadence and load

A five-second observation interval gives a 15-second policy up to two missed heartbeat opportunities. It changes the unchanged-radio upper bound from two updates per hour to 12 per minute, or 720 per hour per connected client, on both local WSS and the v2 radio endpoint. At roughly a few hundred bytes per update, bandwidth is modest, but request count and database writes are material at fleet scale.

Before release, the server should benchmark expected concurrent stations, avoid writing an unchanged full status row when an indexed observation record or last-value upsert is sufficient, and apply small per-client jitter to spread bursts. The cadence should be a negotiated capability with a server-advertised minimum; it must remain comfortably below the website freshness limit. Increasing the website timeout solely to conceal sparse reports is not part of this proposal.

## Reference implementation and tests

The accompanying patch changes only the official client source and tests. It does not alter radio commands. It provides:

- bounded unchanged successful observations;
- immediate failure and same-state recovery evidence;
- process session and poll sequence identity;
- cache-marked WebSocket replay;
- distinct server-delivery results and non-2xx handling;
- additive v2 payload metadata with unchanged v1 payload shape;
- a reference dual-path consumer test covering replay, reordering, browser reconnect, client restart, and loss on either delivery path.

The server storage/API extension and ShackCQ consumer changes are required before production can use the new evidence. This reference patch must not be installed as a private client build or used to relax the current production command gate.

## Separate ShackCQ policy decision

An alternative policy could send an explicitly approved receive-only command first and then require fresh matching dual-path confirmation before showing success, with a freshly captured restoration target and concurrent-operator protection. That is a command-then-confirm policy, not observation evidence. It changes the safety model and creates activity merely to establish liveness. It is documented here only as a separate product decision and is not implemented or approved by this proposal.
