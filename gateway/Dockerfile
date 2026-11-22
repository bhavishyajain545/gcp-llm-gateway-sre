# Build
FROM golang:1.27-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X main.Version=${VERSION}" -o /out/llm-gateway .

# Run: distroless static, non-root; the binary writes nothing to disk.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/llm-gateway /llm-gateway
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/llm-gateway"]
