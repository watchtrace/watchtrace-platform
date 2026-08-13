[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$OutputDirectory,
    [Parameter(Mandatory = $true)][string]$SigningPrivateKey,
    [string]$SigningKeyId = "release-v1"
)
$ErrorActionPreference = "Stop"
$output = [IO.Path]::GetFullPath($OutputDirectory)
New-Item -ItemType Directory -Force -Path $output | Out-Null
foreach ($architecture in @("amd64", "arm64")) {
    docker run --rm -v "${PWD}:/src" -v "${output}:/out" -w /src -e GOOS=linux -e GOARCH=$architecture -e CGO_ENABLED=0 golang:1.26.5 go build -trimpath -ldflags="-s -w" -o "/out/watchtrace-worker-linux-$architecture" ./cmd/worker
    if ($LASTEXITCODE -ne 0) { throw "worker build failed" }
}
docker buildx build --platform linux/amd64,linux/arm64 --file Dockerfile.worker --output "type=oci,dest=$output/watchtrace-worker.oci.tar" .
if ($LASTEXITCODE -ne 0) { throw "multi-architecture image build failed" }
docker run --rm -v "${PWD}:/src:ro" -v "${output}:/out" anchore/syft:v1.29.0 "dir:/src" -o "cyclonedx-json=/out/watchtrace-worker.sbom.cdx.json"
if ($LASTEXITCODE -ne 0) { throw "SBOM generation failed" }
$checksumPath = Join-Path $output "SHA256SUMS"
Set-Content -Encoding ascii -LiteralPath $checksumPath -Value @()
Get-ChildItem -LiteralPath $output -File | Where-Object { $_.Name -notlike "*.sig.json" -and $_.Name -ne "SHA256SUMS" } | ForEach-Object {
    $hash = (Get-FileHash -Algorithm SHA256 -LiteralPath $_.FullName).Hash.ToLowerInvariant()
    "$hash  $($_.Name)" | Add-Content -Encoding ascii $checksumPath
    docker run --rm -v "${PWD}:/src" -v "${output}:/out" -v "${SigningPrivateKey}:/run/signing.key:ro" -w /src golang:1.26.5 go run ./cmd/artifact-sign -mode sign -file "/out/$($_.Name)" -key /run/signing.key -signature "/out/$($_.Name).sig.json" -key-id $SigningKeyId
    if ($LASTEXITCODE -ne 0) { throw "artifact signing failed" }
}
docker run --rm -v "${PWD}:/src" -v "${output}:/out" -v "${SigningPrivateKey}:/run/signing.key:ro" -w /src golang:1.26.5 go run ./cmd/artifact-sign -mode sign -file /out/SHA256SUMS -key /run/signing.key -signature /out/SHA256SUMS.sig.json -key-id $SigningKeyId
if ($LASTEXITCODE -ne 0) { throw "checksum signing failed" }
