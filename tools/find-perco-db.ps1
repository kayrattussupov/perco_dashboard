# Только чтение: ищет, какую базу данных использует PERCo-Web на этом ПК.
# Ничего не меняет. Пароли в выводе скрываются.
# Запуск на ПК с PERCo-Web (PowerShell от администратора):
#   powershell -ExecutionPolicy Bypass -File find-perco-db.ps1 > perco-db.txt

Write-Output "=== Службы (PERCo и СУБД) ==="
Get-CimInstance Win32_Service |
    Where-Object { $_.DisplayName -match 'perco|postgres|mysql|maria|firebird|sql server|nginx|apache|php' -or $_.Name -match 'perco|postgres|mysql|maria|firebird|mssql' } |
    Select-Object Name, DisplayName, State, PathName | Format-List

Write-Output "=== Порты СУБД на прослушивании ==="
Get-NetTCPConnection -State Listen -ErrorAction SilentlyContinue |
    Where-Object { $_.LocalPort -in 3050, 3306, 3307, 5432, 5433, 1433 } |
    ForEach-Object { "{0}:{1}  {2}" -f $_.LocalAddress, $_.LocalPort, (Get-Process -Id $_.OwningProcess -ErrorAction SilentlyContinue).Path }

Write-Output ""
Write-Output "=== Папки PERCo ==="
$roots = @($env:ProgramFiles, ${env:ProgramFiles(x86)}, $env:ProgramData, 'C:\') | Where-Object { $_ }
$dirs = foreach ($r in $roots) { Get-ChildItem -Path $r -Directory -ErrorAction SilentlyContinue | Where-Object { $_.Name -match 'perco' } }
$dirs | ForEach-Object { $_.FullName }

Write-Output ""
Write-Output "=== Файлы настроек с параметрами подключения к базе (пароли скрыты) ==="
$mask = '(?i)(pass(word)?|pwd|secret)(\s*["'']?\s*[:=]\s*["'']?)[^"''\s,;]*'
foreach ($d in $dirs) {
    Get-ChildItem -Path $d.FullName -Recurse -File -Depth 5 -ErrorAction SilentlyContinue -Include *.ini, *.conf, *.cfg, *.json, *.env, *.yaml, *.yml, *.xml, *.php, *.properties |
        Where-Object { $_.Length -lt 512KB } |
        ForEach-Object {
            $hits = Select-String -Path $_.FullName -Pattern '(?i)(db_?host|db_?name|database|dbname|db_?user|dsn|pgsql|postgres|mysql|firebird|port\s*[:=]\s*(5432|3306|3050))' -ErrorAction SilentlyContinue
            if ($hits) {
                Write-Output "--- $($_.FullName)"
                $hits | Select-Object -First 15 | ForEach-Object { '  ' + ($_.Line.Trim() -replace $mask, '$1$3***') }
            }
        }
}
Write-Output ""
Write-Output "Готово. Пришлите файл perco-db.txt."
