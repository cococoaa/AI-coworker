FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY reimburse-agent/go.mod reimburse-agent/go.sum ./
RUN go mod download
COPY reimburse-agent/ ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/reimburse ./cmd/serve

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /app/data && chown -R 1000:1000 /app
WORKDIR /app
COPY --from=build /out/reimburse ./reimburse
COPY --chown=1000:1000 reimburse-agent/data/cases ./data/cases
COPY --chown=1000:1000 reimburse-agent/data/samples ./data/samples
COPY reimburse-agent/config.example.json reimburse-agent/people.json ./
COPY deploy/modelscope/entrypoint.sh ./entrypoint.sh
USER 1000:1000
EXPOSE 7860
ENTRYPOINT ["/bin/sh", "/app/entrypoint.sh"]
