$ErrorActionPreference = 'Stop'

# Change this path to point to the local service executable relative to this script.
$RelativeExePath = '..\build\windows\win_service.exe'

$ServiceName = 'RocketMan_Tun_Service'
$DisplayName = 'RocketMan Tunnel Service'

function Test-IsAdmin {
    $currentIdentity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = New-Object Security.Principal.WindowsPrincipal($currentIdentity)

    return $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Test-IsWindows {
    try {
        return [System.Runtime.InteropServices.RuntimeInformation]::IsOSPlatform(
            [System.Runtime.InteropServices.OSPlatform]::Windows
        )
    } catch {
        return $env:OS -eq 'Windows_NT'
    }
}

if (-not (Test-IsWindows)) {
    Write-Error 'This script can only be run on Windows.'
    exit 1
}

if (-not (Test-IsAdmin)) {
    Write-Host 'Administrator rights are required. Relaunching the script elevated...'
    $scriptPath = $PSCommandPath
    Start-Process -FilePath 'powershell.exe' -Verb RunAs -ArgumentList @(
        '-NoProfile',
        '-ExecutionPolicy', 'Bypass',
        '-File', "`"$scriptPath`""
    ) | Out-Null
    exit 0
}

$scriptDir = Split-Path -Parent $PSCommandPath
$exeCandidate = Join-Path $scriptDir $RelativeExePath

if (-not (Test-Path $exeCandidate)) {
    Write-Error "Executable not found: $exeCandidate"
    exit 1
}

$exePath = (Resolve-Path $exeCandidate).Path
Write-Host "Executable path: $exePath"

$existingService = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if ($existingService) {
    Write-Host "Service $ServiceName already exists. Removing it first..."

    try {
        if ($existingService.Status -ne 'Stopped') {
            Stop-Service -Name $ServiceName -Force -ErrorAction SilentlyContinue
            Start-Sleep -Seconds 1
        }
    } catch {
        Write-Host 'Stopping the existing service failed or it was already stopped.'
    }

    & sc.exe delete $ServiceName | Out-Null
    Start-Sleep -Seconds 2
}

$binPath = '"' + $exePath + '"'

Write-Host "Creating service $ServiceName..."
& sc.exe create $ServiceName binPath= $binPath start= auto DisplayName= "$DisplayName"
if ($LASTEXITCODE -ne 0) {
    Write-Error "Service creation failed with exit code $LASTEXITCODE."
    exit 1
}

Write-Host 'Starting service...'
Start-Service -Name $ServiceName

$status = (Get-Service -Name $ServiceName).Status
Write-Host "Done. Service $ServiceName is installed and its status is $status."