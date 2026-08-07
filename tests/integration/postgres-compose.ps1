[CmdletBinding()]
param(
    [string]$ProjectName = "watchtrace-p1-003-$PID"
)

$ErrorActionPreference = "Stop"

if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
    throw "Docker with the Compose v2 plugin is required."
}

& docker compose version *> $null
if ($LASTEXITCODE -ne 0) {
    throw "Docker with the Compose v2 plugin is required."
}

$repositoryRoot = (Resolve-Path (Join-Path $PSScriptRoot "../..")).Path
$composeFile = Join-Path $repositoryRoot "docker-compose.yml"
$environmentFile = Join-Path ([IO.Path]::GetTempPath()) "$ProjectName-postgres.env"
$previousEnvironmentFile = $env:WATCHTRACE_POSTGRES_ENV_FILE

function Invoke-Compose {
    param([Parameter(ValueFromRemainingArguments = $true)][string[]]$Arguments)

    & docker compose --file $composeFile --project-name $ProjectName @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "docker compose failed: $($Arguments -join ' ')"
    }
}

Set-Content -LiteralPath $environmentFile -Encoding ascii -Value @(
    "POSTGRES_DB=watchtrace_test"
    "POSTGRES_USER=watchtrace_test"
    "POSTGRES_PASSWORD=p1-003-local-test-password"
)
$env:WATCHTRACE_POSTGRES_ENV_FILE = $environmentFile

try {
    Invoke-Compose config --quiet
    Invoke-Compose up --detach --wait postgres
    Invoke-Compose exec --no-TTY postgres psql `
        --username watchtrace_test `
        --dbname watchtrace_test `
        --set ON_ERROR_STOP=1 `
        --command "CREATE TABLE p1_003_persistence_probe (id integer PRIMARY KEY); INSERT INTO p1_003_persistence_probe VALUES (1);"

    # Recreate the container without deleting its named volume.
    Invoke-Compose down
    Invoke-Compose up --detach --wait postgres

    $rowCount = & docker compose `
        --file $composeFile `
        --project-name $ProjectName `
        exec --no-TTY postgres psql `
        --username watchtrace_test `
        --dbname watchtrace_test `
        --tuples-only `
        --no-align `
        --command "SELECT count(*) FROM p1_003_persistence_probe WHERE id = 1;"
    if ($LASTEXITCODE -ne 0) {
        throw "Failed to query the persistence probe after container recreation."
    }
    if ($rowCount.Trim() -ne "1") {
        throw "PostgreSQL data did not persist after container recreation."
    }
}
finally {
    & docker compose `
        --file $composeFile `
        --project-name $ProjectName `
        down --volumes --remove-orphans

    Remove-Item -LiteralPath $environmentFile -Force -ErrorAction SilentlyContinue
    $env:WATCHTRACE_POSTGRES_ENV_FILE = $previousEnvironmentFile
}
