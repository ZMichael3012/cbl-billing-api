FROM golang:1.26-alpine AS build
WORKDIR /src
# Контекст - каталог services/: replace ../cbl-kit и ../cbl-protos требует
# соседние модули.
COPY cbl-kit/go.mod cbl-kit/go.sum ./cbl-kit/
COPY cbl-protos/go.mod cbl-protos/go.sum ./cbl-protos/
COPY cbl-billing-api/go.mod cbl-billing-api/go.sum ./cbl-billing-api/
RUN cd cbl-billing-api && go mod download
COPY cbl-kit ./cbl-kit
COPY cbl-protos ./cbl-protos
COPY cbl-billing-api ./cbl-billing-api
RUN cd cbl-billing-api && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/billing-api ./cmd/billing-api

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/billing-api /app/billing-api
# Каталог миграций нужен сервису (MIGRATIONS_DIR, storagex.Migrate).
COPY --from=build /src/cbl-billing-api/migrations /app/migrations
EXPOSE 8010 9210
ENTRYPOINT ["/app/billing-api"]
