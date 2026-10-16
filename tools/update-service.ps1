# Обновление службы PercoMeal. Запускать от имени администратора.
# Кладёт perco-meal.new.exe на место perco-meal.exe, старый сохраняет как perco-meal.exe.bak.
# Если новая версия не запустилась за 30 с, возвращает старую.
# Файл сохранён в UTF-8 с BOM: без BOM Windows PowerShell 5.1 ломает русские строки.
$ErrorActionPreference = 'Stop'
Set-Location C:\PercoMeal
if (-not (Test-Path .\perco-meal.new.exe)) { throw 'Нет C:\PercoMeal\perco-meal.new.exe: скопируйте туда новую версию' }
Stop-Service PercoMeal
(Get-Service PercoMeal).WaitForStatus('Stopped', '00:00:30')
Copy-Item .\perco-meal.exe .\perco-meal.exe.bak -Force
Move-Item .\perco-meal.new.exe .\perco-meal.exe -Force
try {
    Start-Service PercoMeal
    (Get-Service PercoMeal).WaitForStatus('Running', '00:00:30')
} catch {
    Write-Warning "Новая версия не запустилась: $_. Возвращаю старую."
    Stop-Service PercoMeal -ErrorAction SilentlyContinue
    Copy-Item .\perco-meal.exe.bak .\perco-meal.exe -Force
    Start-Service PercoMeal
    (Get-Service PercoMeal).WaitForStatus('Running', '00:00:30')
    Write-Warning 'Работает прежняя версия. Пришлите конец perco-meal.log.'
}
Get-Service PercoMeal | Format-Table Name, Status -AutoSize
Get-Content .\perco-meal.log -Tail 10