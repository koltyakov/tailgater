# Tailgater

Tail logs from multiple servers at once. Simple as that.

I built this because I was tired of opening a dozen SSH sessions just to watch logs across a cluster. Now I run one command and see everything in one place.

## What it does

- SSH into multiple servers simultaneously
- Stream logs in real-time
- Highlight errors/warnings with colors you configure
- Web dashboard if you prefer a browser over terminal
- Reconnects automatically when connections drop
- Reload config without restarting

## Install

```bash
git clone https://github.com/yourusername/tailgater.git
cd tailgater
go build -o tailgater ./cmd/tailgater
```

Or just `go install ./cmd/tailgater` if you want it in your $GOPATH.

## Quick Start

Copy the example config:

```bash
cp tailgater.yaml.example tailgater.yaml
```

Edit it with your servers:

```yaml
servers:
  - name: web-1
    host: 192.168.1.10
    user: admin
    private_key: ~/.ssh/id_rsa

tail:
  files:
    - /var/log/syslog
  options: "-f -n 100"
```

Run it:

```bash
./tailgater           # CLI mode
./tailgater -web      # Web dashboard on :8080
```

## Config

**Servers** - each needs a name, host, user, and auth (key or password):

```yaml
servers:
  - name: prod-web-1
    host: 10.0.0.5
    user: deploy
    private_key: ~/.ssh/id_rsa_prod
    known_hosts: ~/.ssh/known_hosts
```

**Tail options** - standard tail flags:

```yaml
tail:
  files:
    - /var/log/app.log
    - /var/log/nginx/error.log
  options: "-f -n 0"  # follow, start from now
```

**Colors** - regex patterns with colors:

```yaml
highlights:
  - pattern: '(?i)\b(error|fatal)\b'
    color: red
    bold: true
  - pattern: '(?i)\bwarn\b'
    color: yellow
```

Available colors: `red`, `yellow`, `green`, `cyan`, `blue`, `magenta`, `white`

**Web dashboard**:

```yaml
web:
  enabled: true
  host: 0.0.0.0
  port: 8080
```

## CLI Mode

Default mode - logs stream to your terminal with server prefixes:

```
[web-1] Jan 15 10:30:45 INFO Request completed
[db-1]  Jan 15 10:30:46 ERROR Connection failed
[web-1] Jan 15 10:30:47 WARN High memory usage
```

Flags:
- `-config` - config file path (default: `tailgater.yaml`)
- `-cli` - force CLI mode
- `-web` - run web dashboard
- `-version` - show version

## Web Mode

Open `http://localhost:8080` for a dashboard with:
- Live log streaming (WebSocket)
- Per-server stats
- Connection status indicators
- Error/warning highlighting
- Pause scroll / clear controls

## Development

### Building

```bash
make build
```

### Testing

Run unit tests:

```bash
make test
```

Run e2e tests with Docker containers:

```bash
cd e2e
./setup.sh        # Start test containers
../tailgater -config tailgater.e2e.yaml
./cleanup.sh      # Stop and clean up
```

The e2e setup creates 3 containers that generate realistic logs with errors and warnings, useful for testing both CLI and web modes.

## Security

- Use SSH keys, not passwords
- `chmod 600` your config file (it has server credentials)
- Use `known_hosts` to verify host keys
- Don't commit keys or configs with secrets

## License

MIT
