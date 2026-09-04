# Base images are pinned by digest (tag kept for readability); dependabot bumps
# both parts together.
FROM --platform=$BUILDPLATFORM golang:1.27@sha256:512690a5660563b57d37ecc31129e7f136e831db2aed24a1dbeb8ad7380dc0fa AS builder
ARG TARGETOS TARGETARCH
# VERSION is stamped into karpenter's operator version (startup log line).
ARG VERSION=unspecified
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
    -ldflags="-s -w -X sigs.k8s.io/karpenter/pkg/operator.Version=${VERSION}" \
    -o /out/controller ./cmd/controller

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
LABEL org.opencontainers.image.source="https://github.com/dklesev/karpenter-provider-ssh" \
      org.opencontainers.image.title="karpenter-provider-ssh" \
      org.opencontainers.image.description="Karpenter cloudprovider that scales cluster membership over a pool of SSH-reachable hosts" \
      org.opencontainers.image.licenses="Apache-2.0"
# The image redistributes karpenter core (compiled in), so Apache-2.0 §4(d)
# wants its attribution notices carried along with it — a LABEL claiming the
# license is not the same as shipping its terms.
COPY --from=builder /src/LICENSE /src/NOTICE /
COPY --from=builder /out/controller /controller
USER 65532:65532
ENTRYPOINT ["/controller"]
