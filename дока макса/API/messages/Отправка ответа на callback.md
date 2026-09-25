# Отправка ответа на callback

Для корректной работы ваших чат-ботов и мини-приложений направляйте запросы на домен `platform-api2.max.ru` вместо `platform-api.max.ru`. Также убедитесь, что добавили сертификат Минцифры в список доверенных

POST`/answers`

Отправляет ответ после того, как пользователь нажал на кнопку. Ответом может быть обновленное сообщение и/или одноразовое уведомление для пользователя

#### Ограничения

Можно отправлять не более двух ответов в секунду в один диалог, групповой чат или канал. При превышении этого лимита сообщения следует ставить в очередь или делать задержку перед отправкой

#### Пример запроса:

BASH

`curl -X POST "https://platform-api2.max.ru/answers?callback_id=callback_id" \ -H "Authorization: {access_token}" \ -H "Content-Type: application/json" \ -d '{ "message": { "text": "Это сообщение с кнопкой-ссылкой", "attachments": [ { "type": "inline_keyboard", "payload": { "buttons": [ [ { "type": "link", "text": "Откройте сайт", "url": "https://example.com" } ] ] } } ] } }'`

## [](https://dev.max.ru/docs-api/methods/POST/answers#%D0%90%D0%B2%D1%82%D0%BE%D1%80%D0%B8%D0%B7%D0%B0%D1%86%D0%B8%D1%8F)Авторизация

`access_token`  
apiKey

> Передача токена через query-параметры больше не поддерживается — используйте заголовок `Authorization: <token>`

Токен для вызова HTTP-запросов присваивается при создании бота — его можно найти на [платформе](https://business.max.ru/self) в разделе **Чат-боты** → **Перейти** → **Расширенные настройки** → **Настроить**  
Eсли вы верифицировали профиль и создали бота в [мини-приложении «MAX для бизнеса»](https://max.ru/business_bot?startapp), получить токен можно там же или в [боте «MAX для бизнеса»](https://max.ru/business_bot) с помощью команды **Получить токен**

Рекомендуем не разглашать токен посторонним, чтобы они не получили доступ к управлению ботом. Токен может быть отозван за нарушение Правил платформы

## [](https://dev.max.ru/docs-api/methods/POST/answers#%D0%9F%D0%B0%D1%80%D0%B0%D0%BC%D0%B5%D1%82%D1%80%D1%8B)Параметры

`callback_id`  
string  
^(?!\s*$).+

от `1` символа

Идентификатор кнопки, на которую нажал пользователь

Идентификатор можно получить в обновлениях о событиях через [Webhook](https://dev.max.ru/docs-api/methods/POST/subscriptions) или [Long Polling](https://dev.max.ru/docs-api/methods/GET/updates)

Получение обновлений с помощью [Long Polling](https://dev.max.ru/docs-api/methods/GET/updates) ограничено по скорости и сроку хранения событий — этот способ не подходит для production-окружения. Рекомендуем на всех этапах работы использовать [Webhook](https://dev.max.ru/docs-api/methods/POST/subscriptions)

Когда пользователь нажмёт на кнопку, МАКС отправит событие, содержащее объект [Update](https://dev.max.ru/docs-api/objects/Update) с типом `message_callback` и идентификатором кнопки в поле `updates[i].callback.callback_id`

## [](https://dev.max.ru/docs-api/methods/POST/answers#%D0%A2%D0%B5%D0%BB%D0%BE%20%D0%B7%D0%B0%D0%BF%D1%80%D0%BE%D1%81%D0%B0)Тело запроса

`message`  
object NewMessageBody Nullable optional

Заполните это, если хотите изменить текущее сообщение

## [](https://dev.max.ru/docs-api/methods/POST/answers#%D0%A0%D0%B5%D0%B7%D1%83%D0%BB%D1%8C%D1%82%D0%B0%D1%82)Результат

`success`  
boolean  

`true`, если запрос был успешным, `false` — в противном случае

`message`  
string optional  

Сообщение об ошибке

## [](https://dev.max.ru/docs-api/methods/POST/answers#%D0%9A%D0%BE%D0%B4%D1%8B%20%D0%BE%D1%82%D0%B2%D0%B5%D1%82%D0%BE%D0%B2)Коды ответов

| Код   | Описание                                                                       |
| ----- | ------------------------------------------------------------------------------ |
| `200` | Успешный или неуспешный результат                                              |
| `401` | Ошибка авторизации. Токен `access_token` указан некорректно или недействителен |
| `405` | Метод не разрешен                                                              |
| `500` | Внутренняя ошибка сервера                                                      |