# The disposable Linux arm64 proof container for the whatsapp pack (gp-9hf1),
# on the shape of docs/plans/linux-proof-slack-wecom-20261027.md. gc need not
# run here: the adapter's node suite, the pack's pytest suite, run.sh
# --deps-only and the doctor rows are the proof.
FROM node:22.23.4-bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash ca-certificates curl jq python3 python3-pytest procps \
    && rm -rf /var/lib/apt/lists/*
# The proof runs as the ordinary node user.
RUN mkdir -p /proof && chown node:node /proof
WORKDIR /proof
