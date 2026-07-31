# Gemma 4 OpenVINO N150 bring-up app

## Scope

**Problem:** the target Intel N150 host is a locked-down Piccolo appliance, and
the current production OVMS provider does not package the experimental
llama.cpp Gemma 4 fork or an SSH-controlled GPU bring-up workflow.

**In scope:** a separate SSH-enabled development provider, exact Q4_K_M and
BF16 projector artifacts, explicit OpenVINO GPU preflight, manual model
lifecycle, continuous-batching controls, bounded caches, and cgroup/process
memory telemetry.

**Out of scope:** replacing the production OVMS catalog app, claiming GPU
correctness or speed before N150 evidence exists, XMX claims on N150, further
llama.cpp cache redesign, and changing the production provider selection
without target-side evidence.

## Why model startup is manual

Piccolod grants accelerator devices only to the selected default
`ai.inference.openai.v1` provider. A fresh, unselected provider must still bind
its capability listener without those devices. The container therefore keeps
SSH, health, and the HTTP proxy alive while inference is stopped. After this
app is selected as the default provider, connect over the Piccolo tunnel and
run `gemma start`.

The preflight requires a read-write DRM render node and requires llama.cpp to
report `OpenVINO: using device GPU`. Its current automatic CPU fallback is
treated as an error so a CPU run cannot be mislabeled as a GPU benchmark.

## Build the local image

The image builder refreshes the required targets in the existing OpenVINO build
tree, stages only the four operator tools and their required llama.cpp shared
libraries, then consumes that allowlist, the local llama.cpp worktree, and
OpenVINO Runtime as BuildKit named contexts. It records a digest of uncommitted
build-relevant llama.cpp source changes without committing or pushing them;
diagnostic dumps and session documentation do not perturb that build identity.

```sh
cd /home/abhishek-borar/projects/piccolo/piccolo-ai

LLAMA_CPP_SOURCE=/path/to/gemma4-openvino/llama.cpp \
LLAMA_BUILD_DIR=/path/to/gemma4-openvino/llama.cpp/build/ReleaseOV/bin \
OPENVINO_RUNTIME=/path/to/openvino-toolkit \
IMAGE=ghcr.io/atdexters-lab/piccolo-ai-llama-openvino:0.1.0-dev.2 \
VERSION=0.1.0-dev.2 \
scripts/build-llama-openvino-image.sh
```

The build script loads the image into the local Docker daemon and never pushes.
The standalone manifest is
`deploy/piccolo/gemma4-openvino-dev.app.yaml` and pins the published development
image by both tag and immutable registry digest:

```text
ghcr.io/atdexters-lab/piccolo-ai-llama-openvino:0.1.0-dev.2@sha256:d313eef3ffb56b95445ce9981b0ffb62d3500a73ca909081c77e65e9c1a73384
```

## Target workflow

1. Install the standalone manifest with one SSH public key, one parallel slot,
   a 2048-token context, and a 512 MiB prompt-cache limit.
2. Select this app as the default local AI provider so Piccolod recreates it
   with `/dev/dri`.
3. Connect using the exact SSH listener hostname:

   ```sh
   ssh -o 'ProxyCommand=piccolo tunnel %h' root@<ssh-listener-host>
   ```

4. Run `gemma diagnose`, then `gemma start`.
5. Follow startup with `gemma logs`; inspect readiness and memory with
   `gemma status` and `gemma snapshot`.
6. Keep one slot until short text, vision embedding, and exact OCR pass. Move
   to two slots only for the bounded continuous-batching gate.

`gemma stop` leaves SSH, health, and the capability proxy running.
