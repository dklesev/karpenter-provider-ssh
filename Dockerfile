# Base images are pinned by digest (tag kept for readability) and bumped with
# both parts together: golang by Renovate (exact patch tag, in lockstep with
# go.mod's go line, see .github/renovate.json5), the rest by dependabot.
FROM --platform=$BUILDPLATFORM golang:1.27.4@sha256:fe517acaa580cc7915457080f850c37e618b3c1e750f98b58fc6429b605147a1 AS builder
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
