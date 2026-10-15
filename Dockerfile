FROM golang:1.26-alpine AS build
WORKDIR /src
# Контекст - каталог services/: replace ../cbl-kit требует соседний модуль.
COPY cbl-kit/go.mod cbl-kit/go.sum ./cbl-kit/
COPY cbl-api-gateway/go.mod cbl-api-gateway/go.sum ./cbl-api-gateway/
RUN cd cbl-api-gateway && go mod download
COPY cbl-kit ./cbl-kit
COPY cbl-api-gateway ./cbl-api-gateway
RUN cd cbl-api-gateway && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/api-gateway ./cmd/api-gateway

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/api-gateway /app/api-gateway
EXPOSE 8000 9200
ENTRYPOINT ["/app/api-gateway"]
