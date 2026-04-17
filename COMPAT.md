# Compatibility

This document tracks the minimum fog-next server version required for each fos-next release.

| fos-next | Min fog-next | Boot API version | Notes |
|----------|-------------|-----------------|-------|
| v0.1.0   | main (boot-api branch) | v1 | Initial implementation |

## Boot API contract

The fos-next agent communicates exclusively with the `/fog/api/v1/boot/` endpoints.
The contract covers:

- `POST /fog/api/v1/boot/handshake`
- `POST /fog/api/v1/boot/register`
- `POST /fog/api/v1/boot/progress`
- `POST /fog/api/v1/boot/complete`
- `GET  /fog/api/v1/boot/images/{id}/download?part=N`
- `PUT  /fog/api/v1/boot/images/{id}/upload?part=N`

Breaking changes to these endpoints will bump the fos-next minimum fog-next version.
