FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /rocketchat-mcp .

FROM gcr.io/distroless/static-debian12
COPY --from=builder /rocketchat-mcp /rocketchat-mcp
ENV MCP_PORT=8000
EXPOSE 8000
HEALTHCHECK --interval=30s --timeout=3s CMD ["/rocketchat-mcp", "healthcheck"]
ENTRYPOINT ["/rocketchat-mcp"]
