# DevTunnel

A lightweight HTTP tunneling project written in Go.

DevTunnel allows HTTP requests received by the tunnel server to be forwarded to a locally running application.

## Current Status

### v0.1 — Basic HTTP Tunnel

The first version provides a basic HTTP forwarding pipeline:

    Client
      │
      │ HTTP request
      ▼
    DevTunnel :8080
      │
      │ HTTP request
      ▼
    Local Application :3000
      │
      │ HTTP response
      ▼
    DevTunnel
      │
      ▼
    Client

### Currently Supported

- HTTP request forwarding
- Path forwarding
- Query-string forwarding
- Response forwarding
- Configurable target host through command-line arguments
- Basic request logging
- Missing-target validation

## Usage

Clone the repository:

    git clone https://github.com/Mohammed19N/devtunnel
    cd devtunnel

Run DevTunnel by providing the local application's address:

    go run . localhost:3000

DevTunnel listens on:

    localhost:8080

For example, a request to:

    http://localhost:8080/hello?name=mohammed

will be forwarded to:

    http://localhost:3000/hello?name=mohammed

## Example

Suppose a local application is running on:

    localhost:3000

Start DevTunnel:

    go run . localhost:3000

Then access:

    http://localhost:8080/test

The request will be forwarded to:

    http://localhost:3000/test

## Project Structure

    devtunnel/
    ├── main.go
    ├── localServer/
    │   └── localServer.go
    ├── go.mod
    ├── .gitignore
    └── README.md

## Roadmap

### v0.1 — Basic HTTP Tunnel

- [x] Start HTTP tunnel server
- [x] Forward request paths
- [x] Preserve query strings
- [x] Forward responses
- [x] Configure target through CLI arguments

### v0.2 — Proper HTTP Proxying

- [ ] Preserve HTTP methods
- [ ] Forward request bodies
- [ ] Forward request headers
- [ ] Forward response headers
- [ ] Improve error handling
- [ ] Improve request/response logging

### Future

- [ ] TCP tunneling
- [ ] Persistent connections
- [ ] Concurrent connection handling
- [ ] Public tunnel server
- [ ] Authentication
- [ ] Connection management
- [ ] Graceful shutdown
- [ ] Configuration files
- [ ] Automated tests

## About

DevTunnel is a personal hobby project built with Go.

## License

This project is currently intended for personal use and experimentation.

