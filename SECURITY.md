# Security policy

Please report suspected vulnerabilities privately through GitHub's security
advisory interface for this repository. Do not open a public issue containing a
working exploit, access token, customer request content, or private model data.

The provider intentionally has two external trust boundaries:

- port 8000 validates the app-generated bearer token before forwarding `/v3`;
- port 8001 has no provider API key because Piccolod owns authorization for the
  per-consumer private capability ingress.

The gateway, capability standby handler, and OVMS process inside one provider
container are a single trusted unit. `PICCOLO_AI_API_TOKEN` protects the
external `/v3` boundary; it is not intended to isolate the gateway from its
bundled backend runtime. A future design that executes an untrusted backend must
use a separate container or an equivalent OS-enforced credential boundary.

Ordinary deployments must not publish port 8001 outside Piccolo's protected
listener. Tokens must be injected at runtime and must never be placed in images,
model artifacts, logs, or repository files.

The OVMS base image is tracked by Dependabot. Because Dependabot only tracks the
first `FROM` in this multi-stage Dockerfile, a weekly workflow separately checks
the pinned Go builder against the newest security patch in its Go release
series. A failed check requires updating both `go.mod` and the pinned builder
image before the next release.
