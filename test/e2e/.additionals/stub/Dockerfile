FROM golang:1.26-alpine AS build
WORKDIR /src
COPY test/e2e/.additionals/stub/main.go .
RUN go mod init stub && CGO_ENABLED=0 go build -trimpath -o /out/stub main.go

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/stub /stub
EXPOSE 9000
ENTRYPOINT ["/stub"]
