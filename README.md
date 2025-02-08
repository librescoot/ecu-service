# LibreScoot ECU Service

[![CC BY-NC-SA 4.0][cc-by-nc-sa-shield]][cc-by-nc-sa]

A Go-based service for interfacing with electric scooter Engine Control Units (ECUs). This service provides a unified interface for communicating with different types of ECUs (Bosch and Votol) through CAN bus, with state management via Redis.

## Features

- Support for multiple ECU types:
  - Bosch ECU
  - Votol ECU
- Real-time vehicle metrics monitoring:
  - Speed (km/h)
  - Motor RPM
  - Temperature
  - Voltage
  - Current
  - Odometer
  - Fault codes
- KERS (Kinetic Energy Recovery System) management
- CAN bus communication
- Redis-based state management
- Configurable logging levels

## Installation

1. Clone the repository:
```bash
git clone https://github.com/librescoot/vehicle-service.git
cd vehicle-service
```

2. Build the service:
```bash
make build
```

The compiled binary will be available in the `bin` directory.

## Usage

Run the service with default settings:
```bash
./bin/ecu-service
```

### Command Line Options

- `-version`: Print version information
- `-help`: Display help message
- `-log`: Set log level (0=NONE, 1=ERROR, 2=WARN, 3=INFO, 4=DEBUG)
- `-redis_server`: Redis server address (default: "127.0.0.1")
- `-redis_port`: Redis server port (default: 6379)
- `-can_device`: CAN device name (default: "can0")
- `-ecu_type`: ECU type (bosch or votol)

## Development

### Building

- `make build`: Build for ARM
- `make build-amd64`: Build for AMD64
- `make clean`: Clean build artifacts
- `make lint`: Run linter
- `make test`: Run tests

### Project Structure

- `/ecu`: ECU interface and implementations
- `/bin`: Compiled binaries

## License

This work is licensed under a
[Creative Commons Attribution-NonCommercial-ShareAlike 4.0 International License][cc-by-nc-sa].

[![CC BY-NC-SA 4.0][cc-by-nc-sa-image]][cc-by-nc-sa]

[cc-by-nc-sa]: http://creativecommons.org/licenses/by-nc-sa/4.0/
[cc-by-nc-sa-image]: https://licensebuttons.net/l/by-nc-sa/4.0/88x31.png
[cc-by-nc-sa-shield]: https://img.shields.io/badge/License-CC%20BY--NC--SA%204.0-lightgrey.svg

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

---

Made with ❤️ by the LibreScoot community
