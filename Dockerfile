FROM golang:1.23 AS build
WORKDIR /build
COPY . .
ENV CGO_ENABLED=0
RUN \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -mod=vendor -o actions-cache-go .

FROM scratch
COPY --from=build /build/actions-cache-go /
