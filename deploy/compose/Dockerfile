# Multi-stage static build of moosed. Build context is the repo root.
FROM golang:1.27.3-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/moosed ./cmd/moosed \
 && mkdir -p /out/data/tsnet

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/moosed /moosed
# A fresh named volume copies this ownership, so the nonroot user can write state.
COPY --from=build --chown=65532:65532 /out/data /data
VOLUME ["/data"]
ENTRYPOINT ["/moosed"]
