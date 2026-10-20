# Build context: the repository root (CBL-0.9). The cbl-kit and cbl-protos
# modules are fetched from the GitHub origin; no sibling checkouts are needed.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/billing-api ./cmd/billing-api
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/billing-api /app/billing-api
# The migrations directory is required by the service (MIGRATIONS_DIR, storagex.Migrate).
COPY --from=build /src/migrations /app/migrations
EXPOSE 8010 9210
ENTRYPOINT ["/app/billing-api"]
