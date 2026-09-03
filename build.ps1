$ErrorActionPreference = 'Stop'
go test ./...
go build -trimpath -o siriusxm-sidecar.exe ./cmd/siriusxm-sidecar
Write-Host "Built siriusxm-sidecar.exe"
