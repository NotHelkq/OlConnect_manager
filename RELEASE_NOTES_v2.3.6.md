# OlConnect Manager server-v2.3.6

## Что нового в этом релизе

### 1. Критическое исправление инсталлятора (Issue #57)
- Исправлена ошибка `CONFIG_FILE: unbound variable` при чистой установке и создании не-OpenFlux инстансов (`olcrtc-server`), приводившая к циклическому перезапуску службы.
- Переменная `CONFIG_FILE="./config.yaml"` теперь явно инициализируется в шаблоне launcher в `olcrtc-setup.sh`.

### 2. Поддержка новых транспортов OpenFlux
- Добавлена полная поддержка новых транспортов:
  - **Avito (`avito`, `code.avito.ru`)**: лайв-кодинг платформа Авито.
  - **Cups.online (`cups` / `cupsonline`)**: платформа чемпионатов VK.
  - **Telemost (`telemost`)**: нативный транспорт Телемост внутри OpenFlux со `StripedTransport`.
- Интегрирована валидация совместимости транспортов и автоподстановка имени инстанса в веб-панели (Admin UI).
- Обновлен маппинг хостов и генерация ссылок `openflux://avito`, `openflux://cups`, `openflux://telemost`.
- Автодетекция ссылок в `olcrtc-launcher` при выборе режима `auto`.

### 3. Автоматическое обновление OpenFlux
- Инсталлятор и служба автоматически подтягивают свежие бинарники `openflux-linux-amd64` / `arm64` v1.0.4 с автоматическим PoW-обходом Яндекс SmartCaptcha и поддержкой новых транспортов.

---
### Обновление на сервере

```bash
curl -fsSL https://raw.githubusercontent.com/Oleglog/OlConnect_manager/master/server-install/olcrtc-setup.sh | sudo bash -s -- --update
```
