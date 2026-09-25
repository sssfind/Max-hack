# Подписка на обновления о новых событиях через Webhook

Для корректной работы ваших чат-ботов и мини-приложений направляйте запросы на домен `platform-api2.max.ru` вместо `platform-api.max.ru`. Также убедитесь, что добавили сертификат Минцифры в список доверенных

POST`/subscriptions`

Метод настраивает доставку событий бота через Webhook  — основной механизм получения событий в продуктовых интеграциях. При активной подписке Long Polling не работает

> - Для повышения безопасности **с 25 мая** прекращается поддержка получения вебхуков по HTTP, а также самоподписных сертификатов. Рекомендуем заранее перейти на HTTPS и сертификаты от доверенных центров, в том числе сертификаты Минцифры. Чтобы обновить подписку на события, используйте текущий метод
> - Получение обновлений с помощью [Long Polling](https://dev.max.ru/docs-api/methods/GET/updates) ограничено по скорости и сроку хранения событий — этот способ не подходит для production-окружения. Рекомендуем на всех этапах работы использовать [Webhook](https://dev.max.ru/docs-api/methods/POST/subscriptions)

## [](https://dev.max.ru/docs-api/methods/POST/subscriptions#%D0%9C%D0%BE%D0%B4%D0%B5%D0%BB%D1%8C%20%D0%B4%D0%BE%D1%81%D1%82%D0%B0%D0%B2%D0%BA%D0%B8%20%D1%81%D0%BE%D0%B1%D1%8B%D1%82%D0%B8%D0%B9)Модель доставки событий

После вызова метода `POST /subscriptions` события отправляются на указанный Webhook-endpoint в виде HTTPS POST-запросов с объектом [`Update`](https://dev.max.ru/docs-api/objects/Update)

Как обрабатывается событие:

1. При наступлении события выполняется вызов Webhook-endpoint
2. Выполняется TLS-валидация целевого endpoint для безопасной передачи данных
3. На endpoint отправляется HTTPS-запрос
4. Если при создании подписки указан `secret`, проверяется заголовок `X-Max-Bot-Api-Secret`
5. При успешной валидации возвращается HTTP `200 OK`
6. Выполняется обработка события
7. Инициируются вызовы MAX API

## [](https://dev.max.ru/docs-api/methods/POST/subscriptions#%D0%A2%D1%80%D0%B5%D0%B1%D0%BE%D0%B2%D0%B0%D0%BD%D0%B8%D1%8F%20%D0%BA%20Webhook-endpoint)Требования к Webhook-endpoint

### [](https://dev.max.ru/docs-api/methods/POST/subscriptions#URL%20%D0%B8%20%D0%BF%D0%BE%D1%80%D1%82)URL и порт

Webhook-endpoint должен быть доступен по HTTPS на порту 443. Ваш сервер должен прослушивать этот порт. Порт в URL не указывается:

Код

`https://your-domain.com/webhook`

> Поддерживается только порт 443. Если endpoint недоступен, события не доставляются

### [](https://dev.max.ru/docs-api/methods/POST/subscriptions#%D0%91%D0%B5%D0%B7%D0%BE%D0%BF%D0%B0%D1%81%D0%BD%D0%BE%D1%81%D1%82%D1%8C%20%D1%81%D0%BE%D0%B5%D0%B4%D0%B8%D0%BD%D0%B5%D0%BD%D0%B8%D1%8F%20\(TLS\))Безопасность соединения (TLS)

Перед отправкой событий устанавливается HTTPS-соединение, и Webhook-endpoint проверяется на соответствие следующим требованиям безопасности соединения:

- Webhook-endpoint удостоверен TLS-сертификатом, выданным доверенным центром сертификации, или TLS-сертификатом Минцифры. Самоподписанные сертификаты не поддерживаются
- Доменное имя в URL Webhook-endpoint совпадает с CN или SAN сертификата
- Сервер предоставляет полную цепочку сертификатов

> Если проверка не пройдена, события не доставляются

### [](https://dev.max.ru/docs-api/methods/POST/subscriptions#%D0%9E%D0%B1%D1%80%D0%B0%D0%B1%D0%BE%D1%82%D0%BA%D0%B0%20%D0%B7%D0%B0%D0%BF%D1%80%D0%BE%D1%81%D0%BE%D0%B2)Обработка запросов

Webhook-endpoint должен возвращать **HTTP 200** в течение 30 секунд. Любой другой код ответа или превышение тайм-аута считается ошибкой доставки

#### Пример запроса:

BASH

`curl -X POST "https://platform-api2.max.ru/subscriptions" \ -H "Authorization: {access_token}" \ -H "Content-Type: application/json" \ -d '{ "url": "https://your-domain.com/webhook", "update_types": ["message_created", "bot_started"], "secret": "your_secret" }'`

### [](https://dev.max.ru/docs-api/methods/POST/subscriptions#%D0%9F%D0%BE%D0%BB%D0%B8%D1%82%D0%B8%D0%BA%D0%B0%20%D0%BF%D0%BE%D0%B2%D1%82%D0%BE%D1%80%D0%BD%D1%8B%D1%85%20%D0%BF%D0%BE%D0%BF%D1%8B%D1%82%D0%BE%D0%BA)Политика повторных попыток

Если доставка не удалась, выполняется до 10 повторных попыток с экспоненциально растущим интервалом:

- 1-я попытка: через 60 секунд
- 2-я попытка: через 150 секунд (60 × 2,5)
- 3-я попытка: через 375 секунд (150 × 2,5)
- и так далее

> Если в течение 8 часов от Webhook-endpoint не получен успешный ответ, бот автоматически от него отписывается

## [](https://dev.max.ru/docs-api/methods/POST/subscriptions#%D0%91%D0%B5%D0%B7%D0%BE%D0%BF%D0%B0%D1%81%D0%BD%D0%BE%D1%81%D1%82%D1%8C%20Webhook-%D0%B7%D0%B0%D0%BF%D1%80%D0%BE%D1%81%D0%BE%D0%B2)Безопасность Webhook-запросов

Параметр `secret` позволяет убедиться, что Webhook-запросы приходят от MAX, а не от третьей стороны. Это необязательный параметр, но мы настоятельно рекомендуем указывать его. Проверяйте значение заголовка `X-Max-Bot-Api-Secret` на Webhook-сервере и отклоняйте запросы при несоответствии

Если `secret` указан при создании подписки, он передаётся в заголовке `X-Max-Bot-Api-Secret` каждого Webhook-запроса

---

## [](https://dev.max.ru/docs-api/methods/POST/subscriptions#%D0%A4%D0%BE%D1%80%D0%BC%D0%B0%D1%82%20%D0%B8%20%D1%82%D0%B8%D0%BF%D1%8B%20%D1%81%D0%BE%D0%B1%D1%8B%D1%82%D0%B8%D0%B9)Формат и типы событий

Webhook-запрос содержит объект [`Update`](https://dev.max.ru/docs-api/objects/Update)

Полный список типов событий и структура объекта описаны в разделе [Update](https://dev.max.ru/docs-api/objects/Update)

## [](https://dev.max.ru/docs-api/methods/POST/subscriptions#%D0%90%D0%B2%D1%82%D0%BE%D1%80%D0%B8%D0%B7%D0%B0%D1%86%D0%B8%D1%8F)Авторизация

`access_token`  
apiKey

> Передача токена через query-параметры больше не поддерживается — используйте заголовок `Authorization: <token>`

Токен для вызова HTTP-запросов присваивается при создании бота — его можно найти на [платформе](https://business.max.ru/self) в разделе **Чат-боты** → **Перейти** → **Расширенные настройки** → **Настроить**  
Eсли вы верифицировали профиль и создали бота в [мини-приложении «MAX для бизнеса»](https://max.ru/business_bot?startapp), получить токен можно там же или в [боте «MAX для бизнеса»](https://max.ru/business_bot) с помощью команды **Получить токен**

Рекомендуем не разглашать токен посторонним, чтобы они не получили доступ к управлению ботом. Токен может быть отозван за нарушение Правил платформы

## [](https://dev.max.ru/docs-api/methods/POST/subscriptions#%D0%A2%D0%B5%D0%BB%D0%BE%20%D0%B7%D0%B0%D0%BF%D1%80%D0%BE%D1%81%D0%B0)Тело запроса

`url`  
string  

URL HTTPS-endpoint вашего бота. Должен начинаться с `https://`

`update_types`  
string[] optional  

Пример: `["message_created", "bot_started"]`

Список типов событий, которые хочет получать ваш бот. Полный список смотрите в описании объекта [Update](https://dev.max.ru/docs-api/objects/Update)

`secret`  
string optional  
^[a-zA-Z0-9_-]{5,256}$

от `5` до `256` символов

Cекрет, который должен быть отправлен в заголовке `X-Max-Bot-Api-Secret` в каждом запросе Webhook. Разрешены только символы `A-Z`, `a-z`, `0-9`, и дефис. Заголовок рекомендован, чтобы запрос поступал из установленного веб-узла

## [](https://dev.max.ru/docs-api/methods/POST/subscriptions#%D0%A0%D0%B5%D0%B7%D1%83%D0%BB%D1%8C%D1%82%D0%B0%D1%82)Результат

`success`  
boolean  

`true`, если запрос был успешным, `false` — в противном случае

`message`  
string optional  

Сообщение об ошибке

## [](https://dev.max.ru/docs-api/methods/POST/subscriptions#%D0%9A%D0%BE%D0%B4%D1%8B%20%D0%BE%D1%82%D0%B2%D0%B5%D1%82%D0%BE%D0%B2)Коды ответов

|Код|Описание|
|---|---|
|`200`|Успешный или неуспешный результат|
|`401`|Ошибка авторизации. Токен `access_token` указан некорректно или недействителен|
|`500`|Внутренняя ошибка сервера|