#!/usr/bin/env bash
set -euo pipefail

usage() {
    cat <<'EOF'
Usage:
  LLAMA_CPP_SOURCE=/path/to/llama.cpp \
  LLAMA_BUILD_DIR=/path/to/llama.cpp/build/ReleaseOV/bin \
  OPENVINO_RUNTIME=/path/to/openvino-toolkit \
  IMAGE=piccolo-ai-llama-openvino:dev \
  scripts/build-llama-openvino-image.sh

The script first refreshes the required binaries in the existing OpenVINO build
tree, then packages that exact build and the local llama.cpp source state. It
builds the container locally only; it never pushes an image.
EOF
}

if [[ ${1:-} == "--help" || ${1:-} == "-h" ]]; then
    usage
    exit 0
elif [[ $# -ne 0 ]]; then
    usage >&2
    exit 2
fi

: "${LLAMA_CPP_SOURCE:?set LLAMA_CPP_SOURCE to the llama.cpp worktree}"
: "${OPENVINO_RUNTIME:?set OPENVINO_RUNTIME to the OpenVINO runtime directory}"

image=${IMAGE:-piccolo-ai-llama-openvino:dev}
version=${VERSION:-dev}
llama_source=$(realpath -e "${LLAMA_CPP_SOURCE}")
llama_build_dir=$(realpath -e "${LLAMA_BUILD_DIR:-${llama_source}/build/ReleaseOV/bin}")
llama_cmake_dir=$(dirname "${llama_build_dir}")
openvino_runtime=$(realpath -e "${OPENVINO_RUNTIME}")

for path in \
    "${llama_source}/CMakeLists.txt" \
    "${llama_source}/ggml/src/ggml-openvino" \
    "${llama_cmake_dir}/CMakeCache.txt" \
    "${openvino_runtime}/setupvars.sh" \
    "${openvino_runtime}/runtime/lib/intel64/libopenvino.so.2621" \
    "${openvino_runtime}/runtime/version.txt"; do
    if [[ ! -e "${path}" ]]; then
        echo "required build input is missing: ${path}" >&2
        exit 1
    fi
done

cache_source=$(sed -n 's/^CMAKE_HOME_DIRECTORY:INTERNAL=//p' "${llama_cmake_dir}/CMakeCache.txt")
cache_openvino=$(sed -n 's/^OpenVINO_DIR:PATH=//p' "${llama_cmake_dir}/CMakeCache.txt")
if [[ -z "${cache_source}" || $(realpath -e "${cache_source}") != "${llama_source}" ]]; then
    echo "CMake build tree does not belong to LLAMA_CPP_SOURCE: ${cache_source:-missing}" >&2
    exit 1
fi
expected_openvino=$(realpath -e "${openvino_runtime}/runtime/cmake")
if [[ -z "${cache_openvino}" || $(realpath -e "${cache_openvino}") != "${expected_openvino}" ]]; then
    echo "CMake build tree does not use OPENVINO_RUNTIME: ${cache_openvino:-missing}" >&2
    exit 1
fi
openvino_version=$(tr -d '\r\n' < "${openvino_runtime}/runtime/version.txt")
if [[ -z "${openvino_version}" ]]; then
    echo "OpenVINO runtime version is empty" >&2
    exit 1
fi

set +u
# shellcheck source=/dev/null
. "${openvino_runtime}/setupvars.sh"
set -u
cmake --build "${llama_cmake_dir}" --parallel \
    --target llama-server llama-bench llama-mtmd-debug test-backend-ops

for binary in llama-server llama-bench llama-mtmd-debug test-backend-ops; do
    if [[ ! -x "${llama_build_dir}/${binary}" ]]; then
        echo "required llama.cpp build output is missing: ${llama_build_dir}/${binary}" >&2
        exit 1
    fi
done

llama_revision=$(git -C "${llama_source}" rev-parse HEAD)
piccolo_revision=$(git rev-parse HEAD)
build_date=$(date --utc +%Y-%m-%dT%H:%M:%SZ)

llama_patch_manifest=$(mktemp)
piccolo_patch_manifest=$(mktemp)
llama_runtime_stage=$(mktemp -d)
trap 'rm -f "${llama_patch_manifest}" "${piccolo_patch_manifest}"; rm -rf "${llama_runtime_stage}"' EXIT

runtime_files=(
    "${llama_build_dir}/llama-server"
    "${llama_build_dir}/llama-bench"
    "${llama_build_dir}/llama-mtmd-debug"
    "${llama_build_dir}/test-backend-ops"
    "${llama_build_dir}"/libggml*.so*
    "${llama_build_dir}"/libllama.so*
    "${llama_build_dir}"/libllama-common.so*
    "${llama_build_dir}/libllama-server-impl.so"
    "${llama_build_dir}/libllama-bench-impl.so"
    "${llama_build_dir}"/libmtmd.so*
)
for runtime_file in "${runtime_files[@]}"; do
    if [[ ! -e "${runtime_file}" && ! -L "${runtime_file}" ]]; then
        echo "required llama.cpp runtime file is missing: ${runtime_file}" >&2
        exit 1
    fi
    cp -a -- "${runtime_file}" "${llama_runtime_stage}/"
done

git -C "${llama_source}" diff --binary HEAD -- . ':(exclude)docs/**' ':(exclude)cgraph_ov.txt' > "${llama_patch_manifest}"
while IFS= read -r untracked; do
    case "${untracked}" in
        CMakeLists.txt|*.cmake|*.c|*.cc|*.cpp|*.cxx|*.h|*.hh|*.hpp|*.inl)
            (
                cd "${llama_source}"
                sha256sum -- "${untracked}"
            ) >> "${llama_patch_manifest}"
            ;;
    esac
done < <(git -C "${llama_source}" ls-files --others --exclude-standard | sort)
llama_patch_sha256=$(sha256sum "${llama_patch_manifest}" | awk '{print $1}')

git diff --binary HEAD > "${piccolo_patch_manifest}"
while IFS= read -r untracked; do
    sha256sum "${untracked}" >> "${piccolo_patch_manifest}"
done < <(git ls-files --others --exclude-standard | sort)
piccolo_patch_sha256=$(sha256sum "${piccolo_patch_manifest}" | awk '{print $1}')

echo "image=${image}"
echo "piccolo_ai_revision=${piccolo_revision}"
echo "piccolo_ai_patch_sha256=${piccolo_patch_sha256}"
echo "llama_revision=${llama_revision}"
echo "llama_patch_sha256=${llama_patch_sha256}"
echo "openvino_version=${openvino_version}"

docker buildx build \
    --load \
    --build-context "llama-src=${llama_source}" \
    --build-context "llama-build=${llama_runtime_stage}" \
    --build-context "openvino-runtime=${openvino_runtime}" \
    --file backends/llama-openvino/Dockerfile \
    --tag "${image}" \
    --build-arg "VERSION=${version}" \
    --build-arg "COMMIT=${piccolo_revision}" \
    --build-arg "BUILD_DATE=${build_date}" \
    --build-arg "PICCOLO_AI_PATCH_SHA256=${piccolo_patch_sha256}" \
    --build-arg "LLAMA_REVISION=${llama_revision}" \
    --build-arg "LLAMA_PATCH_SHA256=${llama_patch_sha256}" \
    --build-arg "OPENVINO_VERSION=${openvino_version}" \
    .
