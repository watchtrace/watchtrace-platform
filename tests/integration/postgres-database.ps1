[CmdletBinding()]
param(
    [string]$ProjectName = "watchtrace-p1-005-$PID"
)

$ErrorActionPreference = "Stop"

if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
    throw "Docker with the Compose v2 plugin is required."
}

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw "Go is required."
}

& docker compose version *> $null
if ($LASTEXITCODE -ne 0) {
    throw "Docker with the Compose v2 plugin is required."
}

$repositoryRoot = (Resolve-Path (Join-Path $PSScriptRoot "../..")).Path
$composeFile = Join-Path $repositoryRoot "docker-compose.yml"
$environmentFile = Join-Path ([IO.Path]::GetTempPath()) "$ProjectName-postgres.env"
$previousPostgresEnvironmentFile = $env:WATCHTRACE_POSTGRES_ENV_FILE
$previousPostgresPort = $env:WATCHTRACE_POSTGRES_PORT
$previousDatabaseURL = $env:WATCHTRACE_DATABASE_URL
$previousTestDatabaseURL = $env:WATCHTRACE_TEST_DATABASE_URL

function Invoke-Compose {
    param([Parameter(ValueFromRemainingArguments = $true)][string[]]$Arguments)

    & docker compose --file $composeFile --project-name $ProjectName @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "docker compose failed: $($Arguments -join ' ')"
    }
}

function Invoke-Go {
    param([Parameter(ValueFromRemainingArguments = $true)][string[]]$Arguments)

    & go @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "go failed: $($Arguments -join ' ')"
    }
}

Set-Content -LiteralPath $environmentFile -Encoding ascii -Value @(
    "POSTGRES_DB=watchtrace_test"
    "POSTGRES_USER=watchtrace_test"
    "POSTGRES_PASSWORD=p1-005-local-test-password"
)
$env:WATCHTRACE_POSTGRES_ENV_FILE = $environmentFile
$env:WATCHTRACE_POSTGRES_PORT = "0"

Push-Location $repositoryRoot
try {
    Invoke-Compose config --quiet
    Invoke-Compose up --detach --wait postgres

    $publishedAddress = (& docker compose `
        --file $composeFile `
        --project-name $ProjectName `
        port postgres 5432 | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or $publishedAddress -notmatch ':(\d+)$') {
        throw "Could not determine the PostgreSQL test port."
    }

    $databasePort = $Matches[1]
    $databaseURL = "postgres://watchtrace_test:p1-005-local-test-password@127.0.0.1:$databasePort/watchtrace_test?sslmode=disable"
    $env:WATCHTRACE_DATABASE_URL = $databaseURL
    $env:WATCHTRACE_TEST_DATABASE_URL = $databaseURL

    Invoke-Go run ./cmd/migrate up
    $version = (& go run ./cmd/migrate version | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or $version -ne "version 2 (clean)") {
        throw "Unexpected migration version after up: $version"
    }

    Invoke-Go test ./tests/integration -count=1

    Invoke-Go run ./cmd/migrate down
    $version = (& go run ./cmd/migrate version | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or $version -ne "version 1 (clean)") {
        throw "Unexpected migration version after down: $version"
    }
    $env:WATCHTRACE_EXPECT_OWNERSHIP_SCHEMA_ABSENT = "1"
    Invoke-Go test ./tests/integration -run '^TestOwnershipSchemaRollback$' -count=1
    $env:WATCHTRACE_EXPECT_OWNERSHIP_SCHEMA_ABSENT = $null

    Invoke-Go run ./cmd/migrate up
    Invoke-Go test ./tests/integration -count=1
}
finally {
    & docker compose `
        --file $composeFile `
        --project-name $ProjectName `
        down --volumes --remove-orphans

    Pop-Location
    Remove-Item -LiteralPath $environmentFile -Force -ErrorAction SilentlyContinue
    $env:WATCHTRACE_POSTGRES_ENV_FILE = $previousPostgresEnvironmentFile
    $env:WATCHTRACE_POSTGRES_PORT = $previousPostgresPort
    $env:WATCHTRACE_DATABASE_URL = $previousDatabaseURL
    $env:WATCHTRACE_TEST_DATABASE_URL = $previousTestDatabaseURL
}
