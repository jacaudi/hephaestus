# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
FROM golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# The pinned Topf commit. Never the module proxy, never a tag: the v0.6.2 tag moved after the proxy cached it (see
# internal/third_party/topf/VENDORED.md).
ARG TOPF_SHA=70cdbc83cfbadc023ce4c7c0e3180eedfa577f16

WORKDIR /topf
RUN git clone https://github.com/postfinance/topf . \
    && git checkout "${TOPF_SHA}" \
    && CGO_ENABLED=0 go build -o /usr/local/bin/topf ./cmd/topf \
    && go version -m /usr/local/bin/topf | grep -E 'github.com/siderolabs/talos/pkg/machinery[[:space:]]+v1.14.4'

# The vals stand-in Topf's internal/vals runs: it substitutes the references with hephaestus's own code.
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /usr/local/bin/vals ./build/parity/valsstub
