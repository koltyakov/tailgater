# Tailgater

A multi-SSH log tailing application with real-time streaming and a web dashboard. Monitor logs from multiple servers simultaneously in your terminal or through a modern web interface.

## Features

- 🔌 **Multi-SSH Connections**: Connect to multiple servers simultaneously via SSH
- 📜 **Real-time Log Tailing**: Stream logs from multiple files across all servers
- 🎨 **Syntax Highlighting**: Configurable regex-based highlighting for errors, warnings, and more
- 🖥️ **CLI Mode**: View all logs in a unified terminal output with colored server prefixes
- 🌐 **Web Dashboard**: Modern web interface with real-time log streaming and statistics
- 🔄 **Auto-reconnection**: Automatically reconnects on connection loss
- ⚙️ **Hot-reload Configuration**: Configuration changes are applied without restart

## Installation

```bash
# Clone the repository
git clone https://github.com/yourusername/tailgater.git
cd tailgater

# Build the application
go build -o tailgater ./cmd/tailgater

# Or install directly
go install ./cmd/tailgater
```

## Quick Start

1. Create a configuration file:

```bash
cp tailgater.yaml.example tailgater.yaml
```

2. Edit `tailgater.yaml` with your server details:

```yaml
servers:
  - name: web-server-1
    host: 192.168.1.10
    port: 22
    user: admin
    private_key: ~/.ssh/id_rsa
    known_hosts: ~/.ssh/known_hosts

tail:
  files:
    - /var/log/syslog
    - /var/log/nginx/access.log
  options: "-f -n 100"
```

3. Run in CLI mode:

```bash
./tailgater
```

4. Or run in web mode:

```bash
./tailgater -web
```

## Configuration

The configuration file supports the following options:

### Servers

Each server needs:
- `name`: Unique identifier for the server
- `host`: IP address or hostname
- `port`: SSH port (default: 22)
- `user`: SSH username
- `private_key`: Path to SSH private key
- `password`: SSH password (alternative to key)
- `known_hosts`: Path to known_hosts file
- `insecure_skip_verify`: Skip host key verification (not recommended)

### Tail Configuration

```yaml
tail:
  files:
    - /var/log/syslog
    - /var/log/auth.log
  options: "-f -n 0"  # -f for follow, -n 0 for no initial lines
```

### Highlight Rules

Configure regex patterns with colors:

```yaml
highlights:
  - name: error
    pattern: '(?i)\b(error|fatal|critical)\b'
    color: red
    bold: true

  - name: warning
    pattern: '(?i)\b(warn|warning)\b'
    color: yellow
    bold: true
```

Available colors: `red`, `yellow`, `green`, `cyan`, `blue`, `magenta`, `white`

### Web Dashboard

```yaml
web:
  enabled: true
  host: 0.0.0.0
  port: 8080
```

## Usage

### CLI Mode (Default)

```bash
# Run with default config file (tailgater.yaml or ~/.tailgater.yaml)
./tailgater

# Specify config file
./tailgater -config /path/to/config.yaml

# Force CLI mode
./tailgater -cli
```

Output format:
```
[web-server-1] Jan 15 10:30:45 server app[1234]: INFO Request completed
[db-server-1] Jan 15 10:30:46 db postgres[5678]: ERROR Connection failed
[web-server-1] Jan 15 10:30:47 server app[1234]: WARN High memory usage
```

### Web Mode

```bash
# Run web dashboard
./tailgater -web

# Run both CLI and web
./tailgater -web -cli
```

Access the dashboard at `http://localhost:8080`

Features:
- Real-time log streaming via WebSocket
- Per-server statistics (lines, errors, warnings)
- Visual indicators for server connection status
- Error/warning highlighting
- Auto-scroll with manual clear option

## Command Line Flags

| Flag | Description | Default |
|------|-------------|---------|
| `-config` | Path to configuration file | `tailgater.yaml` |
| `-web` | Run in web dashboard mode | `false` |
| `-cli` | Run in CLI mode | `true` |
| `-version` | Show version information | - |

## Security Considerations

- Always use SSH key authentication when possible
- Set proper file permissions on your config file: `chmod 600 tailgater.yaml`
- Use `known_hosts` file for host key verification
- Keep your private keys secure and never commit them

## License

MIT License - see LICENSE file for details.
