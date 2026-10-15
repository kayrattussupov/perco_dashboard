# Обновление службы PercoMeal. Запускать от имени администратора.
# Кладёт perco-meal.new.exe на место perco-meal.exe, старый сохраняет как perco-meal.exe.bak.
$ErrorActionPreference = 'Stop'
Set-Location C:\PercoMeal
if (-not (Test-Path .\perco-meal.new.exe)) { throw 'Нет perco-meal.new.exe' }
Stop-Service PercoMeal
(Get-Service PercoMeal).WaitForStatus('Stopped', '00:00:30')
Copy-Item .\perco-meal.exe .\perco-meal.exe.bak -Force
Move-Item .\perco-meal.new.exe .\perco-meal.exe -Force
Start-Service PercoMeal
(Get-Service PercoMeal).WaitForStatus('Running', '00:00:30')
Get-Service PercoMeal | Format-Table Name, Status -AutoSize
Get-Content .\perco-meal.log -Tail 5
