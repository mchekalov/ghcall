# ghcall is a one-shot CLI: it runs a single incremental pass and exits, so
# this image has no entrypoint script, no supervisor and no docker CLI — the
# kubernetes agent launcher creates Jobs through the API server instead of
# shelling out to a container runtime.
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first, so a source-only change reuses this layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /ghcall ./cmd/ghcall

# Static, non-root, no shell. CGO_ENABLED=0 above is what makes this work:
# both the sqlite (modernc.org, pure Go) and postgres (pgx) drivers are
# CGO-free.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /ghcall /ghcall

USER nonroot
ENTRYPOINT ["/ghcall"]
