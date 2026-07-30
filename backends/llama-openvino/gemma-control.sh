#!/usr/bin/env bash
set -euo pipefail

supervisor=(/usr/bin/supervisorctl -c /etc/piccolo-ai/supervisord.conf)
log_file=/var/lib/piccolo-ai/logs/gemma.log

usage() {
    cat <<'EOF'
Usage: gemma <command>

Commands:
  start       Validate Intel GPU/OpenVINO and start llama-server
  stop        Gracefully stop llama-server
  restart     Re-run preflight and restart llama-server
  status      Show supervisor state and backend readiness
  logs        Follow the llama-server log
  diagnose    Print the GPU/OpenVINO preflight report
  snapshot    Print cgroup, process, and memory telemetry
  sources     Show the exact packaged source state
EOF
}

command=${1:-status}
case "${command}" in
    start)
        gemma-diagnose --quick
        "${supervisor[@]}" start gemma
        echo "llama-server is loading asynchronously; use 'gemma status' and 'gemma logs'"
        ;;
    stop)
        "${supervisor[@]}" stop gemma
        ;;
    restart)
        gemma-diagnose --quick
        "${supervisor[@]}" stop gemma >/dev/null 2>&1 || true
        "${supervisor[@]}" start gemma
        ;;
    status)
        "${supervisor[@]}" status || true
        if curl --fail --silent --max-time 2 http://127.0.0.1:8080/health >/dev/null; then
            echo "backend_readiness=ready"
        else
            echo "backend_readiness=not_ready"
        fi
        gemma-snapshot --brief
        ;;
    logs)
        touch "${log_file}"
        exec tail -n 200 -F "${log_file}"
        ;;
    diagnose)
        exec gemma-diagnose
        ;;
    snapshot)
        exec gemma-snapshot
        ;;
    sources)
        cat /etc/piccolo-ai/source-state
        ;;
    help|-h|--help)
        usage
        ;;
    *)
        usage >&2
        exit 2
        ;;
esac
