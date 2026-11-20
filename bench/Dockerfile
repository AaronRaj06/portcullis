# One image with every benchmark binary: portcullis, the httputil baseline,
# the backend and vegeta. Built from this repository, versions pinned.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/portcullis ./cmd/portcullis && \
    CGO_ENABLED=0 go build -trimpath -o /out/rproxy ./bench/rproxy && \
    CGO_ENABLED=0 go build -trimpath -o /out/backend ./bench/backend && \
    CGO_ENABLED=0 GOBIN=/out go install github.com/tsenart/vegeta/v12@v12.13.0

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
COPY bench/portcullis.yaml bench/portcullis-failover.yaml /etc/portcullis/
USER nonroot:nonroot
