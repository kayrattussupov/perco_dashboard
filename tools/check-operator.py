"""Проверка оператора PERCo для службы perco-meal (только чтение, ничего не меняет).

Запуск:  python tools/check-operator.py [логин] [адрес PERCo]
Пароль спрашивается в консоли и нигде не сохраняется.
"""
import getpass
import json
import sys
import urllib.error
import urllib.request

login = sys.argv[1] if len(sys.argv) > 1 else "service"
base = (sys.argv[2] if len(sys.argv) > 2 else "http://192.168.1.106").rstrip("/") + "/api"
password = getpass.getpass(f"Пароль оператора {login}: ")


def call(path, body=None, token=None):
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(base + path, data=data, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            return r.status, json.load(r)
    except urllib.error.HTTPError as e:
        return e.code, None


status, out = call("/system/auth", {"login": login, "password": password})
if not out or not out.get("token"):
    print(f"НЕТ: вход не удался (HTTP {status}). Проверьте логин и пароль.")
    sys.exit(1)
token = out["token"]
print("ОК: вход выполнен")

checks = [
    ("список сотрудников", "/users/staff/fullList"),
    ("карточка сотрудника", "/users/staff/271"),
    ("события", "/eventsystem?beginDatetime=2026-10-15+07:00:00&endDatetime=2026-10-15+07:01:00&page=1&rows=5&sidx=id&sord=asc"),
    ("шаблоны доступа", "/accessTemplates/3"),
    ("состояние устройств", "/devices/state"),
    ("ошибки передачи устройству", "/devices/1/errors"),
]
bad = 0
for name, path in checks:
    status, _ = call(path, token=token)
    print(("ОК  " if status == 200 else "НЕТ ") + f"{name} (HTTP {status})")
    bad += status != 200
print()
print("Права на изменение блокировки отсюда не проверить (скрипт ничего не меняет).")
print("Проверьте их в режиме ТЕСТ на карте 960: блокировка должна появиться и сняться.")
sys.exit(1 if bad else 0)
