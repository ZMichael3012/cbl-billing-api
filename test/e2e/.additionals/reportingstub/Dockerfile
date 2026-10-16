FROM golang:1.26-alpine AS build
WORKDIR /src
# Context - the services/ directory: the stub is part of the cbl-billing-api
# module and uses its replace dependencies (cbl-protos, cbl-kit).
COPY cbl-kit/go.mod cbl-kit/go.sum ./cbl-kit/
COPY cbl-protos/go.mod cbl-protos/go.sum ./cbl-protos/
COPY cbl-billing-api/go.mod cbl-billing-api/go.sum ./cbl-billing-api/
RUN cd cbl-billing-api && go mod download
COPY cbl-kit ./cbl-kit
COPY cbl-protos ./cbl-protos
COPY cbl-billing-api ./cbl-billing-api
RUN cd cbl-billing-api && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/reportingstub ./test/e2e/.additionals/reportingstub

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/reportingstub /reportingstub
EXPOSE 9000
ENTRYPOINT ["/reportingstub"]
