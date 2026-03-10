FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY go.mod main.go ./
RUN go get github.com/mark3labs/mcp-go@latest && \
    go mod tidy && \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /rocketchat-mcp .

FROM gcr.io/distroless/static-debian12
COPY --from=builder /rocketchat-mcp /rocketchat-mcp
ENV MCP_PORT=8000
EXPOSE 8000
ENTRYPOINT ["/rocketchat-mcp"]
