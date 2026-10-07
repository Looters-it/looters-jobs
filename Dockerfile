FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/jobs ./cmd/jobs

FROM alpine:3.22
RUN adduser -D -u 10001 jobs
COPY --from=build /out/jobs /usr/local/bin/jobs
USER jobs
ENTRYPOINT ["jobs"]
