param(
    [string]$Version = "dev",
    [string]$OutDir = "dist"
)

$ErrorActionPreference = "Stop"
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null

$env:CGO_ENABLED = "0"

$env:GOOS = "windows"
$env:GOARCH = "amd64"
go build -trimpath -ldflags "-s -w -X main.version=$Version" -o "$OutDir/aprsnet-client-windows-amd64.exe" ./cmd/aprsnet-client

$env:GOOS = "linux"
$env:GOARCH = "amd64"
go build -trimpath -ldflags "-s -w -X main.version=$Version" -o "$OutDir/aprsnet-client-linux-amd64" ./cmd/aprsnet-client

Write-Host "Built APRSNET client binaries in $OutDir"
