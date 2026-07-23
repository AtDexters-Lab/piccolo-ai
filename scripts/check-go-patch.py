#!/usr/bin/env python3
"""Fail when go.mod is not on the newest stable patch in its Go series."""

import json
import pathlib
import re
import sys
import urllib.request


def version_tuple(version: str) -> tuple[int, int, int]:
    match = re.fullmatch(r"go(\d+)\.(\d+)(?:\.(\d+))?", version)
    if match is None:
        raise ValueError(f"unsupported Go version: {version}")
    return tuple(int(part or 0) for part in match.groups())


root = pathlib.Path(__file__).resolve().parents[1]
match = re.search(r"^go (\d+\.\d+\.\d+)$", (root / "go.mod").read_text(), re.M)
if match is None:
    sys.exit("go.mod must pin a full Go patch version")

configured = version_tuple(f"go{match.group(1)}")
dockerfile = (root / "backends" / "ovms" / "Dockerfile").read_text()
builder_match = re.search(
    r"^FROM golang:(\d+\.\d+\.\d+)-bookworm@sha256:[0-9a-f]{64} AS build$",
    dockerfile,
    re.M,
)
if builder_match is None:
    sys.exit("OVMS Dockerfile must pin the Go builder tag and digest")

builder = version_tuple(f"go{builder_match.group(1)}")
if builder != configured:
    sys.exit(
        "Go version mismatch: "
        f"go.mod pins go{configured[0]}.{configured[1]}.{configured[2]}; "
        f"the Docker builder pins go{builder[0]}.{builder[1]}.{builder[2]}"
    )

with urllib.request.urlopen(
    "https://go.dev/dl/?mode=json&include=all", timeout=30
) as response:
    releases = json.load(response)

candidates = []
for release in releases:
    if not release.get("stable"):
        continue
    try:
        candidate = version_tuple(release["version"])
    except ValueError:
        continue
    if candidate[:2] == configured[:2]:
        candidates.append(candidate)
if not candidates:
    sys.exit(f"no stable Go releases found for {configured[0]}.{configured[1]}")

latest = max(candidates)
if configured != latest:
    sys.exit(
        "Go patch is stale: "
        f"go{configured[0]}.{configured[1]}.{configured[2]} is pinned; "
        f"go{latest[0]}.{latest[1]}.{latest[2]} is current"
    )

print(f"Go patch is current: go{configured[0]}.{configured[1]}.{configured[2]}")
