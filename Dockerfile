FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY execution/ ./execution/
COPY cmd/ ./cmd/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /alert-example ./cmd/alert-example

FROM scratch
COPY --from=build /alert-example /alert-example
USER 65532:65532
ENTRYPOINT ["/alert-example"]
