FROM golang:1.24-bookworm AS build

WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux \
    go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/sentrymcp \
    ./cmd/sentrymcp

RUN mkdir -p /out/data \
    && touch /out/data/.keep \
    && chown -R 65532:65532 /out/data

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /data

COPY --from=build /out/sentrymcp /usr/local/bin/sentrymcp
COPY --from=build --chown=65532:65532 /out/data /data

USER 65532:65532

EXPOSE 8808
VOLUME ["/data"]

ENTRYPOINT ["/usr/local/bin/sentrymcp"]



CMD ["-http", "0.0.0.0:8808", "-dir", "/data"]
