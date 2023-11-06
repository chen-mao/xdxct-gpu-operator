#!/bin/bash

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
source ${SCRIPT_DIR}/.definitions.sh

echo "Disabling GPU Operator operands"
kubectl label nodes --all "xdxct.com/gpu.deploy.operands=false" --overwrite
