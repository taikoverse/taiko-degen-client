#!/bin/bash

set -eou pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" >/dev/null && pwd)"


if [ "$DISABLE_P2P_SYNC" == "false" ]; then
    $DIR/../bin/taiko-client driver \
        --l1.ws ${L1_ENDPOINT_WS} \
        --l2.ws ws://127.0.0.1:8546 \
        --l2.auth http://127.0.0.1:8551 \
        --taikoL1 ${TAIKO_L1_ADDRESS} \
        --taikoL2 ${TAIKO_L2_ADDRESS} \
        --jwtSecret /data/taiko-geth/geth/jwtsecret \
        --p2p.syncVerifiedBlocks \
        --p2p.checkPointSyncUrl https://rpc.test.taiko.xyz \
        --p2p.syncTimeout "5000"
else
    $DIR/../bin/taiko-client driver \
        --l1.ws ${L1_ENDPOINT_WS} \
        --l2.ws ws://127.0.0.1:8546 \
        --l2.auth http://127.0.0.1:8551 \
        --taikoL1 ${TAIKO_L1_ADDRESS} \
        --taikoL2 ${TAIKO_L2_ADDRESS} \
        --jwtSecret /data/taiko-geth/geth/jwtsecret
fi
