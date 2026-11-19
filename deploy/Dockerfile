# Builds one binary (BIN) into a distroless, non-root image.
FROM golang:1.23 AS build
ARG BIN=portcullis
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/AaronRaj06/portcullis/internal.Version=${VERSION}" \
    -o /out/app ./cmd/${BIN}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER nonroot:nonroot
ENTRYPOINT ["/app"]
