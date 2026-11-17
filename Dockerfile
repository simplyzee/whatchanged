# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH VERSION=dev COMMIT=none
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
    -o /out/whatchanged ./cmd/whatchanged

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/whatchanged /whatchanged
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/whatchanged"]
CMD ["server"]
