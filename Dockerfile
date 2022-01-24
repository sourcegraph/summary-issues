FROM golang:1.17-alpine as builder
WORKDIR /build
COPY go.mod *.sum *.go ./
RUN go build -o summary-issues

FROM alpine:3.15
COPY --from=builder /build/summary-issues /usr/local/bin/
ENTRYPOINT ["summary-issues"]
