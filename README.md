# Piccolo AI

Piccolo AI packages optimized local inference providers for Piccolo OS. Every
backend exposes the same stable, OpenAI-compatible capability while retaining
its own runtime, accelerator support, model formats, and release cadence.

The first backend is OpenVINO Model Server (OVMS) for Intel CPU, GPU, and NPU
systems. Future backends can live beside it in this repository and publish
independent images such as `piccolo-ai-vllm-rocm`.

## OVMS provider

The OVMS image contains a small Piccolo gateway plus the pinned upstream OVMS
runtime. Model weights are not baked into the image. Piccolod mounts a selected
Hugging Face or OCI artifact read-only at `/models/model`.

```text
remote client --Bearer token--> :8000 gateway -----+
                                                    v
Piccolod private capability ingress --> :8001 capability surface --> OVMS
```

When a configured accelerator has not yet been granted, the capability surface
stays up and returns `503` while the application remains healthy. Piccolod can
then commit and recreate the selected provider with its accelerator devices.

The public model name is always `piccolo-chat`. See
[`docs/provider-contract.md`](docs/provider-contract.md) for the complete port,
authentication, configuration, and lifecycle contract.

## Development

Requirements:

- Go 1.26 or newer
- Docker for the container build and runtime smoke test

Run the fast checks:

```sh
make check
```

Build the provider image:

```sh
make image
```

Run it with an OpenVINO-format generative model:

```sh
docker run --rm \
  -p 8000:8000 \
  -p 127.0.0.1:8001:8001 \
  -e PICCOLO_AI_API_TOKEN='replace-with-a-long-random-token' \
  -e PICCOLO_AI_TARGET_DEVICE=CPU \
  -v /absolute/path/to/model:/models/model:ro \
  piccolo-ai-ovms:dev
```

Then query the stable model alias:

```sh
curl http://127.0.0.1:8000/v3/models \
  -H 'Authorization: Bearer replace-with-a-long-random-token'
```

`/healthz` reports intrinsic provider health. `/readyz` reports whether the
model can currently serve inference.

Port `8001` is published in this example only for direct local verification. A
Piccolo installation routes it through the protected capability listener.

## Images

Release automation publishes the OVMS provider to:

```text
ghcr.io/atdexters-lab/piccolo-ai-ovms
```

Images are built for `linux/amd64`. Release builds publish an exact semantic
version tag; store manifests should pin that tag or its digest.

## License

Piccolo AI source is licensed under AGPL-3.0-only. The derived OVMS image also
contains Apache-2.0-licensed OpenVINO Model Server components; see
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md).
