FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /nyann-bench ./cmd/nyann-bench/
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /nyann-bench-api ./cmd/nyann-bench-api/

FROM scratch
COPY --from=build /nyann-bench /nyann-bench
COPY --from=build /nyann-bench-api /nyann-bench-api
USER 65532:65532
ENTRYPOINT ["/nyann-bench"]
