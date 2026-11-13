# syntax=docker/dockerfile:1
FROM docker.io/library/golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/llmgate ./cmd/llmgate

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/llmgate /usr/local/bin/llmgate
COPY config.example.yaml /etc/llmgate/config.example.yaml
ENV LLMGATE_CONFIG=/etc/llmgate/config.yaml
EXPOSE 8080
USER nonroot:nonroot
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 CMD ["llmgate", "healthcheck"]
ENTRYPOINT ["llmgate"]
