#requires -RunAsAdministrator
<#
Installs tor (Tor Expert Bundle) as a Windows service plus torbridge with scheduled tasks.

tor-expert-bundle-windows-x86_64-*.tar.gz and torbridge.exe must be next to this script.
No internet is needed for the installation itself; bridges are selected right after it.

    .\install.ps1               install or upgrade (an existing torrc is kept)
    .\install.ps1 -ResetTorrc   also rewrite the base torrc
    .\install.ps1 -Uninstall    remove the service, the tasks and C:\Tor

Exit codes: 0 installed and connected, 3 installed but no working bridges yet, 1 error.
#>
param(
    [string]$Root = 'C:\Tor',
    [switch]$ResetTorrc,
    [switch]$Uninstall
)
$ErrorActionPreference = 'Stop'
$here = $PSScriptRoot
$exe = Join-Path $Root 'torbridge.exe'

# Exit relays are restricted to these countries; edit and re-run with -ResetTorrc to change.
$ExitNodes = '{de},{nl},{ch},{at},{se},{no},{fi},{dk},{is},{fr},{be},{lu},{ie},{gb},{ee},{lt},{lv},{pl},{cz},{ca},{us}'

function Step($n, $text) { Write-Host "[$n/6] $text" -ForegroundColor Cyan }

function Remove-TorBridge {
    Unregister-ScheduledTask -TaskPath '\TorBridge\' -TaskName Auto -Confirm:$false -ErrorAction SilentlyContinue
    Unregister-ScheduledTask -TaskPath '\TorBridge\' -TaskName UpdateNow -Confirm:$false -ErrorAction SilentlyContinue
    Unregister-ScheduledTask -TaskPath '\TorBridge\' -TaskName Stats -Confirm:$false -ErrorAction SilentlyContinue
    if (Get-Service tor -ErrorAction SilentlyContinue) {
        Stop-Service tor -Force -ErrorAction SilentlyContinue
        sc.exe delete tor | Out-Null
    }
    $path = [Environment]::GetEnvironmentVariable('Path', 'Machine')
    $new = ($path -split ';' | Where-Object { $_ -and $_ -ne $Root }) -join ';'
    if ($new -ne $path) { [Environment]::SetEnvironmentVariable('Path', $new, 'Machine') }
}

try {
    if ($Uninstall) {
        Write-Host 'Removing the tor service and scheduled tasks...'
        Remove-TorBridge
        Remove-Item $Root -Recurse -Force -ErrorAction SilentlyContinue
        Write-Host "Removed. $Root deleted." -ForegroundColor Green
        exit 0
    }

    Step 1 'Checking installation files'
    $bundle = Get-ChildItem $here -Filter 'tor-expert-bundle-windows-x86_64-*.tar.gz' | Sort-Object Name | Select-Object -Last 1
    if (!$bundle) { throw "tor-expert-bundle-windows-x86_64-*.tar.gz not found next to the script" }
    if (!(Test-Path "$here\torbridge.exe")) { throw "torbridge.exe not found next to the script" }
    Get-ChildItem $here -File | Unblock-File
    Write-Host "      $($bundle.Name), torbridge.exe"

    Step 2 "Unpacking Tor to $Root"
    if (Get-Service tor -ErrorAction SilentlyContinue) { Stop-Service tor -Force }
    foreach ($d in '', 'state', 'logs') { New-Item -ItemType Directory -Force -Path (Join-Path $Root $d) | Out-Null }
    tar.exe -xzf $bundle.FullName -C $Root
    if ($LASTEXITCODE) { throw "cannot unpack $($bundle.Name)" }
    Copy-Item "$here\torbridge.exe" $exe -Force
    # 1.3 had a separate bridgestat tool and history file; keep the collected history.
    Remove-Item (Join-Path $Root 'bridgestat.exe') -Force -ErrorAction SilentlyContinue
    if ((Test-Path "$Root\logs\bridgestat.jsonl") -and !(Test-Path "$Root\logs\history.jsonl")) {
        Move-Item "$Root\logs\bridgestat.jsonl" "$Root\logs\history.jsonl"
    }
    Write-Host "      $(& "$Root\tor\tor.exe" --version | Select-Object -First 1)"

    Step 3 'Writing configuration'
    if (!(Test-Path "$Root\torrc-defaults")) { Set-Content "$Root\torrc-defaults" '' -Encoding ascii }
    $torrc = Join-Path $Root 'torrc'
    if ($ResetTorrc -or !(Test-Path $torrc)) {
        $r = $Root -replace '\\', '/'
        @"
# Base tor config (install.ps1). Bridges live in bridges.conf, written by torbridge.
DataDirectory $r/state
GeoIPFile $r/data/geoip
GeoIPv6File $r/data/geoip6
SocksPort 127.0.0.1:9050
HTTPTunnelPort 127.0.0.1:9080
ControlPort 127.0.0.1:9051
CookieAuthentication 1
Log notice file $r/logs/tor.log
AvoidDiskWrites 1
ExitNodes $ExitNodes
StrictNodes 1
%include $r/bridges.conf
"@ | Set-Content $torrc -Encoding ascii
        Write-Host "      $torrc written"
    } else {
        Write-Host "      $torrc kept (use -ResetTorrc to rewrite)"
    }
    if (!(Test-Path "$Root\bridges.conf")) { Set-Content "$Root\bridges.conf" "# no bridges yet: run torbridge update`nUseBridges 1" -Encoding ascii }
    # tor runs as LocalService (S-1-5-19) and writes only to state and logs.
    icacls $Root /inheritance:r /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' '*S-1-5-19:(OI)(CI)RX' '*S-1-5-32-545:(OI)(CI)RX' | Out-Null
    foreach ($d in 'state', 'logs') { icacls (Join-Path $Root $d) /grant '*S-1-5-19:(OI)(CI)M' | Out-Null }

    Step 4 'Registering the tor service'
    if (!(Get-Service tor -ErrorAction SilentlyContinue)) {
        $bin = "`"$Root\tor\tor.exe`" --nt-service -f `"$torrc`" --defaults-torrc `"$Root\torrc-defaults`""
        $cred = New-Object System.Management.Automation.PSCredential('NT AUTHORITY\LocalService', (New-Object System.Security.SecureString))
        New-Service -Name tor -DisplayName 'Tor' -Description 'Tor client with bridges managed by torbridge' -BinaryPathName $bin -Credential $cred -StartupType Automatic | Out-Null
    }
    sc.exe config tor start= delayed-auto | Out-Null
    sc.exe failure tor reset= 86400 actions= restart/60000/restart/60000/restart/300000 | Out-Null
    Write-Host '      service "tor": automatic start, restart on failure'

    Step 5 'Registering scheduled tasks'
    $principal = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest
    $settings = New-ScheduledTaskSettingsSet -StartWhenAvailable -RunOnlyIfNetworkAvailable -AllowStartIfOnBatteries `
        -DontStopIfGoingOnBatteries -ExecutionTimeLimit (New-TimeSpan -Hours 2) -MultipleInstances IgnoreNew `
        -RestartCount 6 -RestartInterval (New-TimeSpan -Hours 1)
    $boot = New-ScheduledTaskTrigger -AtStartup
    $boot.Delay = 'PT10M'
    $triggers = @((New-ScheduledTaskTrigger -Daily -At '10:00'), $boot)
    Register-ScheduledTask -TaskPath '\TorBridge\' -TaskName Auto -Force -Principal $principal -Settings $settings -Trigger $triggers `
        -Action (New-ScheduledTaskAction -Execute $exe -Argument "auto -root `"$Root`"" -WorkingDirectory $Root) `
        -Description 'torbridge: refresh bridges if they are older than a week or the connection is down' | Out-Null
    Register-ScheduledTask -TaskPath '\TorBridge\' -TaskName UpdateNow -Force -Principal $principal -Settings $settings `
        -Action (New-ScheduledTaskAction -Execute $exe -Argument "update -root `"$Root`"" -WorkingDirectory $Root) `
        -Description 'torbridge: find new bridges now (schtasks /run /tn \TorBridge\UpdateNow)' | Out-Null
    Unregister-ScheduledTask -TaskPath '\TorBridge\' -TaskName Stats -Confirm:$false -ErrorAction SilentlyContinue
    $path = [Environment]::GetEnvironmentVariable('Path', 'Machine')
    if (($path -split ';') -notcontains $Root) { [Environment]::SetEnvironmentVariable('Path', "$path;$Root", 'Machine') }
    Write-Host '      \TorBridge\Auto: daily at 10:00 and 10 min after boot; \TorBridge\UpdateNow: on demand'

    Step 6 'Finding working bridges (this takes a few minutes)'
    & $exe update -root $Root
    if ($LASTEXITCODE -ne 0) {
        Write-Host "`nInstalled, but the Tor connection is not up yet (code $LASTEXITCODE)." -ForegroundColor Yellow
        Write-Host 'The scheduled task \TorBridge\Auto will retry when the network is available.'
        exit 3
    }
    Write-Host "`nTor is running: SOCKS 127.0.0.1:9050, HTTP proxy 127.0.0.1:9080" -ForegroundColor Green
    exit 0
} catch {
    Write-Host "`nERROR: $($_.Exception.Message)" -ForegroundColor Red
    exit 1
}
