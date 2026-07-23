# Provider contract

Piccolo AI is a multi-backend repository. Each published backend image follows
the same boundary even when its native inference runtime differs.

## Network contract

| Port | Owner | Purpose | Authentication |
| --- | --- | --- | --- |
| `8000` | Piccolo gateway | Primary listener and remote OpenAI-compatible API | Bearer token on `/v3` |
| `8001` | Backend runtime | Piccolod capability listener | No backend credential; Piccolod owns private-ingress authorization |

The gateway serves `/healthz` and a small root status response. In a Piccolo app
manifest, only `/v3` and `/v3/` are marked `public`; every other primary-listener
path remains protected by Piccolo authentication. The backend listener remains
protected for ordinary LAN and remote access.

The gateway and bundled backend are one trusted container unit. The bearer
token protects external `/v3` access; it is not an isolation boundary between
those two same-container processes.

The gateway removes its bearer credential, cookies, forwarding headers, and
Piccolo identity headers before forwarding a request. It streams request and
response bodies and propagates cancellation rather than buffering multimodal or
long-running generation payloads.

## Model contract

- The externally visible model identifier is always `piccolo-chat`.
- The selected model artifact is mounted read-only at `/models/model`.
- Backend compilation data is reconstructible and written to `/var/cache/ovms`.
- A model update changes the mounted artifact, not the provider image contract.

## Configuration

| Variable | Required | Default | Meaning |
| --- | --- | --- | --- |
| `PICCOLO_AI_API_TOKEN` | Yes | none | Bearer token accepted by the port 8000 gateway; minimum 24 characters |
| `PICCOLO_AI_TARGET_DEVICE` | No | `AUTO:GPU,CPU` | OVMS/OpenVINO target device expression |
| `PICCOLO_AI_MODEL_PATH` | No | `/models/model` | Read-only model directory |
| `PICCOLO_AI_CACHE_DIR` | No | `/var/cache/ovms` | Writable compilation cache directory |
| `PICCOLO_AI_LOG_LEVEL` | No | `INFO` | `DEBUG`, `INFO`, or `ERROR` |
| `PICCOLO_AI_MAX_REQUEST_BYTES` | No | `268435456` | Maximum request body size in bytes |
| `PICCOLO_AI_MAX_CONCURRENT_REQUESTS` | No | `32` | Maximum admitted `/v3` requests; excess requests receive `429` |
| `PICCOLO_AI_MAX_REQUEST_DURATION` | No | `30m` | Maximum lifetime of one `/v3` request, using Go duration syntax |
| `PICCOLO_AI_MAX_REQUEST_UPLOAD_DURATION` | No | `5m` | Maximum time to upload request headers and body |

`PICCOLO_AI_OVMS_BINARY` exists only for development and test substitution. It
is not an app-manifest tuning surface.

The defaults leave room for multimodal request bodies and continuous batching
while bounding resource use on a small appliance. The gateway streams accepted
bodies and responses; it does not buffer the body up to the size limit.

## Lifecycle contract

The provider executable is PID 1. It starts OVMS as one process group and owns
the gateway server. If OVMS exits, the gateway shuts down and the container
fails. On `SIGTERM` or `SIGINT`, the gateway stops accepting work, OVMS receives
`SIGTERM`, and the process group is killed after a bounded grace period.
The complete gateway-drain and backend-stop sequence uses an eight-second
budget so it finishes inside the container runtime's ordinary stop grace.

`/healthz` reports ready only when OVMS returns success from the
model-specific `/v2/models/piccolo-chat/ready` endpoint. During model loading,
model failure, or backend failure it returns `503`.
