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
$previousExpectedAuthSchemaAbsent = $env:WATCHTRACE_EXPECT_AUTH_SCHEMA_ABSENT
$previousExpectedMonitorSchemaAbsent = $env:WATCHTRACE_EXPECT_MONITOR_SCHEMA_ABSENT
$previousExpectedSchedulerSchemaAbsent = $env:WATCHTRACE_EXPECT_SCHEDULER_SCHEMA_ABSENT
$previousExpectedCheckerSchemaAbsent = $env:WATCHTRACE_EXPECT_CHECKER_SCHEMA_ABSENT
$previousExpectedProductionAuthSchemaAbsent = $env:WATCHTRACE_EXPECT_PRODUCTION_AUTH_SCHEMA_ABSENT
$previousExpectedEmailVerificationSchemaAbsent = $env:WATCHTRACE_EXPECT_EMAIL_VERIFICATION_SCHEMA_ABSENT
$previousExpectedPasswordResetSchemaAbsent = $env:WATCHTRACE_EXPECT_PASSWORD_RESET_SCHEMA_ABSENT
$previousExpectedMembershipSchemaAbsent = $env:WATCHTRACE_EXPECT_MEMBERSHIP_SCHEMA_ABSENT
$previousExpectedReliableEngineSchemaAbsent = $env:WATCHTRACE_EXPECT_RELIABLE_ENGINE_SCHEMA_ABSENT
$previousExpectedReliabilityReportingSchemaAbsent = $env:WATCHTRACE_EXPECT_RELIABILITY_REPORTING_SCHEMA_ABSENT
$previousExpectedIncidentNotificationSchemaAbsent = $env:WATCHTRACE_EXPECT_INCIDENT_NOTIFICATION_SCHEMA_ABSENT
$previousExpectedBackendPhase14SchemaAbsent = $env:WATCHTRACE_EXPECT_BACKEND_PHASE14_SCHEMA_ABSENT

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
    if ($LASTEXITCODE -ne 0 -or $version -ne "version 14 (clean)") {
        throw "Unexpected migration version after up: $version"
    }

    Invoke-Go test ./tests/integration -count=1

    Invoke-Go run ./cmd/migrate down
    $version = (& go run ./cmd/migrate version | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or $version -ne "version 13 (clean)") {
        throw "Unexpected migration version after down: $version"
    }
    $env:WATCHTRACE_EXPECT_BACKEND_PHASE14_SCHEMA_ABSENT = "1"
    Invoke-Go test ./tests/integration -run '^TestBackendPhase14SchemaRollback$' -count=1
    $env:WATCHTRACE_EXPECT_BACKEND_PHASE14_SCHEMA_ABSENT = $previousExpectedBackendPhase14SchemaAbsent

    Invoke-Go run ./cmd/migrate down
    $version = (& go run ./cmd/migrate version | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or $version -ne "version 12 (clean)") {
        throw "Unexpected migration version after second down: $version"
    }
    $env:WATCHTRACE_EXPECT_INCIDENT_NOTIFICATION_SCHEMA_ABSENT = "1"
    Invoke-Go test ./tests/integration -run '^TestIncidentNotificationSchemaRollback$' -count=1
    $env:WATCHTRACE_EXPECT_INCIDENT_NOTIFICATION_SCHEMA_ABSENT = $previousExpectedIncidentNotificationSchemaAbsent

    Invoke-Go run ./cmd/migrate down
    $version = (& go run ./cmd/migrate version | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or $version -ne "version 11 (clean)") {
        throw "Unexpected migration version after second down: $version"
    }
    $env:WATCHTRACE_EXPECT_RELIABILITY_REPORTING_SCHEMA_ABSENT = "1"
    Invoke-Go test ./tests/integration -run '^TestReliabilityReportingSchemaRollback$' -count=1
    $env:WATCHTRACE_EXPECT_RELIABILITY_REPORTING_SCHEMA_ABSENT = $previousExpectedReliabilityReportingSchemaAbsent

    Invoke-Go run ./cmd/migrate down
    $version = (& go run ./cmd/migrate version | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or $version -ne "version 10 (clean)") {
        throw "Unexpected migration version after second down: $version"
    }
    $env:WATCHTRACE_EXPECT_RELIABLE_ENGINE_SCHEMA_ABSENT = "1"
    Invoke-Go test ./tests/integration -run '^TestReliableEngineSchemaRollback$' -count=1
    $env:WATCHTRACE_EXPECT_RELIABLE_ENGINE_SCHEMA_ABSENT = $previousExpectedReliableEngineSchemaAbsent

    Invoke-Go run ./cmd/migrate down
    $version = (& go run ./cmd/migrate version | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or $version -ne "version 9 (clean)") {
        throw "Unexpected migration version after second down: $version"
    }
    $env:WATCHTRACE_EXPECT_MEMBERSHIP_SCHEMA_ABSENT = "1"
    Invoke-Go test ./tests/integration -run '^TestMembershipTenantSecuritySchemaRollback$' -count=1
    $env:WATCHTRACE_EXPECT_MEMBERSHIP_SCHEMA_ABSENT = $previousExpectedMembershipSchemaAbsent

    Invoke-Go run ./cmd/migrate down
    $version = (& go run ./cmd/migrate version | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or $version -ne "version 8 (clean)") {
        throw "Unexpected migration version after second down: $version"
    }
    $env:WATCHTRACE_EXPECT_PASSWORD_RESET_SCHEMA_ABSENT = "1"
    Invoke-Go test ./tests/integration -run '^TestPasswordResetSchemaRollback$' -count=1
    $env:WATCHTRACE_EXPECT_PASSWORD_RESET_SCHEMA_ABSENT = $previousExpectedPasswordResetSchemaAbsent

    Invoke-Go run ./cmd/migrate down
    $version = (& go run ./cmd/migrate version | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or $version -ne "version 7 (clean)") {
        throw "Unexpected migration version after third down: $version"
    }
    $env:WATCHTRACE_EXPECT_EMAIL_VERIFICATION_SCHEMA_ABSENT = "1"
    Invoke-Go test ./tests/integration -run '^TestEmailVerificationSchemaRollback$' -count=1
    $env:WATCHTRACE_EXPECT_EMAIL_VERIFICATION_SCHEMA_ABSENT = $previousExpectedEmailVerificationSchemaAbsent

    Invoke-Go run ./cmd/migrate down
    $version = (& go run ./cmd/migrate version | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or $version -ne "version 6 (clean)") {
        throw "Unexpected migration version after fourth down: $version"
    }
    $env:WATCHTRACE_EXPECT_PRODUCTION_AUTH_SCHEMA_ABSENT = "1"
    Invoke-Go test ./tests/integration -run '^TestProductionAuthSchemaRollback$' -count=1
    $env:WATCHTRACE_EXPECT_PRODUCTION_AUTH_SCHEMA_ABSENT = $previousExpectedProductionAuthSchemaAbsent

    Invoke-Go run ./cmd/migrate down
    $version = (& go run ./cmd/migrate version | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or $version -ne "version 5 (clean)") {
        throw "Unexpected migration version after fifth down: $version"
    }
    $env:WATCHTRACE_EXPECT_CHECKER_SCHEMA_ABSENT = "1"
    Invoke-Go test ./tests/integration -run '^TestHTTPCheckWorkerSchemaRollback$' -count=1
    $env:WATCHTRACE_EXPECT_CHECKER_SCHEMA_ABSENT = $previousExpectedCheckerSchemaAbsent

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
    $env:WATCHTRACE_EXPECT_AUTH_SCHEMA_ABSENT = $previousExpectedAuthSchemaAbsent
    $env:WATCHTRACE_EXPECT_MONITOR_SCHEMA_ABSENT = $previousExpectedMonitorSchemaAbsent
    $env:WATCHTRACE_EXPECT_SCHEDULER_SCHEMA_ABSENT = $previousExpectedSchedulerSchemaAbsent
    $env:WATCHTRACE_EXPECT_CHECKER_SCHEMA_ABSENT = $previousExpectedCheckerSchemaAbsent
    $env:WATCHTRACE_EXPECT_PRODUCTION_AUTH_SCHEMA_ABSENT = $previousExpectedProductionAuthSchemaAbsent
    $env:WATCHTRACE_EXPECT_EMAIL_VERIFICATION_SCHEMA_ABSENT = $previousExpectedEmailVerificationSchemaAbsent
    $env:WATCHTRACE_EXPECT_PASSWORD_RESET_SCHEMA_ABSENT = $previousExpectedPasswordResetSchemaAbsent
    $env:WATCHTRACE_EXPECT_MEMBERSHIP_SCHEMA_ABSENT = $previousExpectedMembershipSchemaAbsent
    $env:WATCHTRACE_EXPECT_RELIABLE_ENGINE_SCHEMA_ABSENT = $previousExpectedReliableEngineSchemaAbsent
	$env:WATCHTRACE_EXPECT_RELIABILITY_REPORTING_SCHEMA_ABSENT = $previousExpectedReliabilityReportingSchemaAbsent
	$env:WATCHTRACE_EXPECT_INCIDENT_NOTIFICATION_SCHEMA_ABSENT = $previousExpectedIncidentNotificationSchemaAbsent
	$env:WATCHTRACE_EXPECT_BACKEND_PHASE14_SCHEMA_ABSENT = $previousExpectedBackendPhase14SchemaAbsent
}
