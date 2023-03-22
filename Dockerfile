FROM  golang:1.18 AS builder

ENV CGO_ENABLED=1
WORKDIR /app
COPY . .
RUN go mod tidy
RUN go build -o /app/milvus-cdc /app/server/main/main.go

FROM alpine:3.17
WORKDIR /app
COPY --from=builder /app/milvus-backup .
COPY --from=builder /app/configs ./configs
EXPOSE 8080
ENTRYPOINT ["milvus-cdc"]