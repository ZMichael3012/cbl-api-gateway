# Build context: the repository root (CBL-0.9). The cbl-kit and cbl-protos
# modules are fetched from the GitHub origin; no sibling checkouts are needed.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/api-gateway ./cmd/api-gateway
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/api-gateway /app/api-gateway
EXPOSE 8000 9200
ENTRYPOINT ["/app/api-gateway"]
