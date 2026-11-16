#!/usr/bin/env bash
# Delete the local kind cluster.
set -euo pipefail
kind delete cluster --name kubescope
