#!/usr/bin/env bash
set -euo pipefail

state_root=/var/lib/piccolo-ai
runtime_root=/run/piccolo-ai
ssh_root="${state_root}/ssh"
log_root="${state_root}/logs"
cache_root="${state_root}/openvino-cache"
authorized_keys="${runtime_root}/authorized_keys"
host_key="${ssh_root}/ssh_host_ed25519_key"

install -d -m 0755 "${state_root}" "${runtime_root}" "${ssh_root}" /run/sshd /run/nginx
install -d -o developer -g developer -m 0755 \
    "${log_root}" \
    "${cache_root}" \
    /home/developer \
    /run/nginx/client_body \
    /run/nginx/proxy \
    /run/nginx/fastcgi \
    /run/nginx/scgi \
    /run/nginx/uwsgi

public_key=${PICCOLO_AI_SSH_PUBLIC_KEY:-}
if [[ -z "${public_key}" || "${public_key}" == *$'\n'* || "${public_key}" == *$'\r'* ]]; then
    echo "PICCOLO_AI_SSH_PUBLIC_KEY must contain exactly one OpenSSH public-key line" >&2
    exit 1
fi

key_file=$(mktemp "${runtime_root}/public-key.XXXXXX")
host_key_work=$(mktemp -d "${ssh_root}/.host-key.XXXXXX")
cleanup() {
    rm -f -- "${key_file}"
    rm -rf -- "${host_key_work}"
}
trap cleanup EXIT
printf '%s\n' "${public_key}" > "${key_file}"
chmod 0600 "${key_file}"
if ! ssh-keygen -l -f "${key_file}" >/dev/null 2>&1; then
    echo "PICCOLO_AI_SSH_PUBLIC_KEY is not a valid OpenSSH public key" >&2
    exit 1
fi
install -o root -g root -m 0644 "${key_file}" "${authorized_keys}"

if [[ -s "${host_key}" ]] && ssh-keygen -y -f "${host_key}" > "${host_key_work}/derived.pub" 2>/dev/null; then
    install -o root -g root -m 0644 "${host_key_work}/derived.pub" "${host_key_work}/public"
    mv -f "${host_key_work}/public" "${host_key}.pub"
else
    echo "Generating a replacement SSH host key"
    ssh-keygen -q -t ed25519 -N "" -f "${host_key_work}/generated"
    install -o root -g root -m 0600 "${host_key_work}/generated" "${host_key_work}/private"
    install -o root -g root -m 0644 "${host_key_work}/generated.pub" "${host_key_work}/public"
    mv -f "${host_key_work}/private" "${host_key}"
    mv -f "${host_key_work}/public" "${host_key}.pub"
fi
chmod 0600 "${host_key}"
chmod 0644 "${host_key}.pub"
/usr/sbin/sshd -t -f /etc/piccolo-ai/sshd_config

echo "Piccolo AI Gemma 4 bring-up container initialized"
cat /etc/piccolo-ai/source-state
echo "Model startup is intentionally manual. Connect as root over SSH and run: gemma start"

cleanup
trap - EXIT
exec /usr/bin/tini -- /usr/bin/supervisord -c /etc/piccolo-ai/supervisord.conf
