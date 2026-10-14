FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/billing-api ./cmd/billing-api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/billing-api /app/billing-api
EXPOSE 8010 9210
ENTRYPOINT ["/app/billing-api"]