FROM golang:1.27.1 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /agent ./cmd/agent

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /agent /agent
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/agent"]
