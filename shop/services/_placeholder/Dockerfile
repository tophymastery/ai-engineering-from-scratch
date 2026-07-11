# Canonical container build for the placeholder service. Std-lib only, so the
# build stage needs nothing but the Go toolchain and the runtime is scratch.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 go build -o /out/placeholder .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/placeholder /placeholder
EXPOSE 8081
USER nonroot:nonroot
ENTRYPOINT ["/placeholder"]
