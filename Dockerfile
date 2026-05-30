ARG GO_VERSION=1.24

FROM golang:${GO_VERSION}-alpine AS builder
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/dynatrace-exporter .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/dynatrace-exporter /dynatrace-exporter
EXPOSE 8000
USER nonroot:nonroot
ENTRYPOINT ["/dynatrace-exporter"]
