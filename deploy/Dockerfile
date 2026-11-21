# syntax=docker/dockerfile:1
# Builds one binary (BIN) into a distroless, non-root image.
FROM golang:1.27 AS build
ARG BIN=portcullis
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
# Only the Go sources: editing docs or deploy files does not invalidate the build.
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/AaronRaj06/portcullis/internal.Version=${VERSION}" \
    -o /out/app ./cmd/${BIN}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
# Numeric UID (distroless "nonroot"): Kubernetes' runAsNonRoot can only verify a number.
USER 65532:65532
ENTRYPOINT ["/app"]
