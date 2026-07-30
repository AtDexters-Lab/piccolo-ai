#!/usr/bin/env bash
set -euo pipefail

language_model=${PICCOLO_AI_LANGUAGE_MODEL:-/models/language/gemma-4-E4B-it-Q4_K_M.gguf}
projector_model=${PICCOLO_AI_PROJECTOR_MODEL:-/models/projector/mmproj-BF16.gguf}
parallel_slots=${PICCOLO_AI_PARALLEL_SLOTS:-1}
context_size=${PICCOLO_AI_CONTEXT_SIZE:-2048}
batch_size=${PICCOLO_AI_BATCH_SIZE:-512}
ubatch_size=${PICCOLO_AI_UBATCH_SIZE:-128}
cache_ram_mib=${PICCOLO_AI_CACHE_RAM_MIB:-512}

validate_integer() {
    local name=$1
    local value=$2
    local minimum=$3
    local maximum=$4
    if [[ ! "${value}" =~ ^[0-9]+$ ]] || (( value < minimum || value > maximum )); then
        echo "${name} must be an integer from ${minimum} through ${maximum}; got ${value}" >&2
        exit 2
    fi
}

validate_integer PICCOLO_AI_PARALLEL_SLOTS "${parallel_slots}" 1 4
validate_integer PICCOLO_AI_CONTEXT_SIZE "${context_size}" 512 32768
validate_integer PICCOLO_AI_BATCH_SIZE "${batch_size}" 32 2048
validate_integer PICCOLO_AI_UBATCH_SIZE "${ubatch_size}" 32 1024
validate_integer PICCOLO_AI_CACHE_RAM_MIB "${cache_ram_mib}" 0 2048
if (( ubatch_size > batch_size )); then
    echo "PICCOLO_AI_UBATCH_SIZE cannot exceed PICCOLO_AI_BATCH_SIZE" >&2
    exit 2
fi

for model in "${language_model}" "${projector_model}"; do
    if [[ ! -r "${model}" ]]; then
        echo "required model artifact is not readable: ${model}" >&2
        exit 1
    fi
done

# The backend currently warns and falls back to CPU when GPU is unavailable.
# Reject that state before loading several GiB of model weights so a benchmark
# can never silently claim GPU results while actually running on CPU.
gemma-diagnose --quick

export GGML_OPENVINO_DEVICE=GPU
export GGML_OPENVINO_CACHE_DIR=${GGML_OPENVINO_CACHE_DIR:-/var/lib/piccolo-ai/openvino-cache}
export GGML_OPENVINO_CACHE_MAX_ENTRIES=${GGML_OPENVINO_CACHE_MAX_ENTRIES:-5}

echo "starting llama-server with explicit OpenVINO GPU selection"
echo "model=${language_model}"
echo "mmproj=${projector_model}"
echo "parallel_slots=${parallel_slots} context_size=${context_size} batch_size=${batch_size} ubatch_size=${ubatch_size} cache_ram_mib=${cache_ram_mib}"

exec llama-server \
    --model "${language_model}" \
    --mmproj "${projector_model}" \
    --alias piccolo-chat \
    --host 127.0.0.1 \
    --port 8080 \
    --gpu-layers all \
    --parallel "${parallel_slots}" \
    --ctx-size "${context_size}" \
    --batch-size "${batch_size}" \
    --ubatch-size "${ubatch_size}" \
    --cache-ram "${cache_ram_mib}" \
    --kv-unified \
    --cont-batching \
    --cache-prompt \
    --metrics \
    --slots \
    --timeout 3600
