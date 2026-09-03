# SiriusXM Sidecar

This version replaces the Python `sxm.py` runtime with the Go `sxm` package. The SiriusXM client, HLS HTTP server, channel discovery, playlist rewriting, segment processing, and metadata handling are compiled into the sidecar executable.

## Runtime requirements

- Go is required only to build.
- FFmpeg is still required at runtime because the STR-facing endpoint converts the internal live AAC/HLS stream to MP3.
- A SiriusXM account is supplied on the command line.

## Build

Windows PowerShell:

```powershell
./build.ps1
```

Linux/macOS:

```bash
./build.sh
```

The Windows build creates `siriusxm-sidecar.exe`; Linux creates `siriusxm-sidecar`.

## Run

```powershell
.\siriusxm-sidecar.exe USERNAME PASSWORD
```

Defaults:

- STR API: `0.0.0.0:8091`
- Internal HLS server: `127.0.0.1:9998`

**Ensure port 9998 is free, otherwise it will not work**

The internal HLS server is only used by FFmpeg. STR receives a LAN URL such as:

`http://192.168.1.75:8091/api/stations/Pop2K/stream`

## Configuration

`SIDECAR_HOST` — public listen address, default `0.0.0.0`

`SIDECAR_PORT` — public port, default `8091`

`SIDECAR_LAN_IP` — LAN address used in generated stream URLs

`SIDECAR_BASE_URL` — complete public base URL, overrides automatic LAN URL construction

`SXM_HLS_HOST` — internal HLS listen address, default `127.0.0.1`

`SXM_HLS_PORT` — internal HLS port, default `9998`

`FFMPEG` — FFmpeg executable/path, default `ffmpeg`

## API

- `GET /api/health`
- `GET /api/stations`
- `GET /api/stations/{id}`
- `GET /api/stations/{id}/now-playing`
- `GET /api/stations/{id}/stream`

The station list comes from SiriusXM channel discovery.

## License

SiriusXM Sidecar is licensed under the MIT License. *See [License](LICENSE)*

### Disclaimer

This project is provided **“as is” and without warranty of any kind**. Use of this software may involve risks to your account or access to third-party services. The authors and contributors are not responsible for any account restrictions, service interruptions, or other consequences resulting from your use of this project.

**You are solely responsible for ensuring that your use of this software complies with all applicable laws, regulations, and third-party terms of service. Use at your own risk.**
