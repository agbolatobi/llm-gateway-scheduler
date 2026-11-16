# Override only with an immutable, tested llama.cpp server image (e.g. the GPU pin).
ARG LLAMA_IMAGE=ghcr.io/ggml-org/llama.cpp:server-b11669@sha256:8ffe0d28ff5261cc2f78ca1f67db1515bc2529b904a3c25f5b24f4cedfc3411d
FROM ${LLAMA_IMAGE}
ENV HOME=/tmp
USER 10001:10001
ENTRYPOINT ["/app/llama-server"]
