#!/usr/bin/env bash
set -uo pipefail

quick=false
if [[ ${1:-} == "--quick" ]]; then
    quick=true
elif [[ $# -ne 0 ]]; then
    echo "usage: gemma-diagnose [--quick]" >&2
    exit 2
fi

failed=0
echo "timestamp=$(date --iso-8601=seconds)"
echo "kernel=$(uname -srmo)"
echo "user=$(id)"
echo
echo "[packaged sources]"
cat /etc/piccolo-ai/source-state

echo
echo "[DRM devices]"
if compgen -G '/dev/dri/*' >/dev/null; then
    ls -l /dev/dri
else
    echo "ERROR: /dev/dri is not present; this app is not the selected GPU provider" >&2
    failed=1
fi

render_nodes=()
while IFS= read -r node; do
    render_nodes+=("${node}")
done < <(compgen -G '/dev/dri/renderD*' || true)
if [[ ${#render_nodes[@]} -eq 0 ]]; then
    echo "ERROR: no DRM render node is available" >&2
    failed=1
else
    for node in "${render_nodes[@]}"; do
        if [[ -r "${node}" && -w "${node}" ]]; then
            echo "render_node=${node} access=read-write"
        else
            echo "ERROR: render node ${node} is not read-write for $(id -un)" >&2
            failed=1
        fi
    done
fi

echo
echo "[PCI display devices]"
lspci -Dnnk 2>&1 | sed -n '/VGA compatible controller\\|Display controller\\|3D controller/,+3p'

echo
echo "[OpenCL]"
clinfo -l 2>&1 || true

echo
echo "[llama.cpp OpenVINO device selection]"
device_output=$(GGML_OPENVINO_DEVICE=GPU llama-bench --list-devices 2>&1)
device_status=$?
printf '%s\n' "${device_output}"
if [[ ${device_status} -ne 0 ]] || ! grep -Fq "OpenVINO: using device GPU" <<<"${device_output}"; then
    echo "ERROR: explicit OpenVINO GPU selection did not resolve to GPU; CPU fallback is rejected" >&2
    failed=1
fi

if ! "${quick}"; then
    echo
    gemma-snapshot
fi

exit "${failed}"
