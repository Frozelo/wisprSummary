# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Pure-Go build (SQLite driver is CGO-free), static binary.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bot ./cmd/bot \
 && mkdir /out/data

# Distroless static: CA certificates included, no shell, runs as non-root (uid 65532).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/bot /bot
# State (SQLite + Wispr session) lives here; mount a named volume on it.
COPY --from=build --chown=65532:65532 /out/data /data
ENV DATA_DIR=/data
VOLUME /data
USER 65532:65532
ENTRYPOINT ["/bot"]
CMD ["run"]
