# PercoMeal service update. Run as administrator in C:\PercoMeal.
# Puts perco-meal.new.exe in place of perco-meal.exe, keeps the old one as perco-meal.exe.bak.
# If the new version does not start within 30 s, the old one is restored.
# ASCII only on purpose: Windows PowerShell 5.1 misreads non-ASCII text in files saved without BOM.
$ErrorActionPreference = 'Stop'
Set-Location C:\PercoMeal
if (-not (Test-Path .\perco-meal.new.exe)) { throw 'No C:\PercoMeal\perco-meal.new.exe - copy the new version there first' }
Stop-Service PercoMeal
(Get-Service PercoMeal).WaitForStatus('Stopped', '00:00:30')
Copy-Item .\perco-meal.exe .\perco-meal.exe.bak -Force
Move-Item .\perco-meal.new.exe .\perco-meal.exe -Force
try {
    Start-Service PercoMeal
    (Get-Service PercoMeal).WaitForStatus('Running', '00:00:30')
    Write-Host 'UPDATED: new version is running' -ForegroundColor Green
} catch {
    Write-Warning "New version did not start: $_ - restoring the old one"
    Stop-Service PercoMeal -ErrorAction SilentlyContinue
    Copy-Item .\perco-meal.exe.bak .\perco-meal.exe -Force
    Start-Service PercoMeal
    (Get-Service PercoMeal).WaitForStatus('Running', '00:00:30')
    Write-Warning 'ROLLED BACK: the old version is running. Send the end of perco-meal.log.'
}
Get-Service PercoMeal | Format-Table Name, Status -AutoSize
Get-Content .\perco-meal.log -Tail 10