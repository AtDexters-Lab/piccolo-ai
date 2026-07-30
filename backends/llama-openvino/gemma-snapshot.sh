#!/usr/bin/env bash
set -uo pipefail

brief=false
if [[ ${1:-} == "--brief" ]]; then
    brief=true
elif [[ $# -ne 0 ]]; then
    echo "usage: gemma-snapshot [--brief]" >&2
    exit 2
fi

echo "timestamp=$(date --iso-8601=seconds)"
if [[ -r /sys/fs/cgroup/memory.current ]]; then
    echo "cgroup_memory_current=$(cat /sys/fs/cgroup/memory.current)"
fi
if [[ -r /sys/fs/cgroup/memory.peak ]]; then
    echo "cgroup_memory_peak=$(cat /sys/fs/cgroup/memory.peak)"
fi
if [[ -r /sys/fs/cgroup/memory.swap.current ]]; then
    echo "cgroup_swap_current=$(cat /sys/fs/cgroup/memory.swap.current)"
fi
free -b

if "${brief}"; then
    pgrep -a llama-server || true
    exit 0
fi

echo
echo "[cgroup memory.stat]"
if [[ -r /sys/fs/cgroup/memory.stat ]]; then
    cat /sys/fs/cgroup/memory.stat
else
    echo "unavailable"
fi

echo
echo "[processes by RSS]"
ps -eo pid,ppid,user,stat,rss,vsz,etimes,comm,args --sort=-rss | head -n 25

while IFS= read -r pid; do
    echo
    echo "[llama-server pid=${pid} status]"
    sed -n '/^Name:/p;/^State:/p;/^VmPeak:/p;/^VmSize:/p;/^VmHWM:/p;/^VmRSS:/p;/^RssAnon:/p;/^RssFile:/p;/^RssShmem:/p;/^VmSwap:/p;/^Threads:/p' "/proc/${pid}/status"
    if [[ -r /proc/${pid}/smaps_rollup ]]; then
        echo "[llama-server pid=${pid} smaps_rollup]"
        cat "/proc/${pid}/smaps_rollup"
    fi
done < <(pgrep -x llama-server || true)

echo
echo "[OpenVINO compiled cache]"
du -sh /var/lib/piccolo-ai/openvino-cache 2>/dev/null || echo "unavailable"
find /var/lib/piccolo-ai/openvino-cache -maxdepth 1 -type f -printf '%f %s bytes\n' 2>/dev/null | sort || true
