# Update

Объект `Update` описывает возможные события в чате или канале. Может возвращаться в следующих случаях:

- Вы подписались на обновления через Webhook — при наступлении события МАКС пришлёт [POST-запрос `/subscriptions`](https://dev.max.ru/docs-api/methods/POST/subscriptions), который содержит объект `Update`
    
- Вы отправили [GET-запрос /updates](https://dev.max.ru/docs-api/methods/GET/updates) для получения обновлений через  Long Polling — в ответ вернётся объект `Update`
    

> Получение обновлений с помощью [Long Polling](https://dev.max.ru/docs-api/methods/GET/updates) ограничено по скорости и сроку хранения событий — этот способ не подходит для production-окружения. Рекомендуем на всех этапах работы использовать [Webhook](https://dev.max.ru/docs-api/methods/POST/subscriptions)

## [](https://dev.max.ru/docs-api/objects/Update#%D0%A2%D0%B8%D0%BF%D1%8B%20%D1%81%D0%BE%D0%B1%D1%8B%D1%82%D0%B8%D0%B9)Типы событий

- `bot_added` — бот добавлен в чат или канал

- `bot_started` — пользователь впервые начал общение с ботом или возобновил после остановки — нажал соответствующую кнопку в настройках бота в МАКС

- `bot_stopped` — пользователь остановил или удалил бота через настройки бота в МАКС. Во втором случае одновременно с `bot_stopped` возвращается событие `dialog_removed`

- `bot_removed` — бот удалён из чата или канала

- `chat_title_changed` — пользователь изменил название чата или канала

- `dialog_cleared` — пользователь очистил историю диалога с ботом

- `dialog_muted` — пользователь отключил уведомления в диалоге с ботом

- `dialog_unmuted` — пользователь включил уведомления в диалоге с ботом

- `dialog_removed` — пользователь удалил диалог с ботом. Вместе с этим событием одновременно возвращается `bot_stopped` — при удалении диалога бот останавливается автоматически

- `message_callback` — пользователь нажал на кнопку в чате или канале

- `message_created` — пользователь отправил новое сообщение или опубликовал пост

- `message_edited` — пользователь отредактировал сообщение в чате или канале

- `message_removed` — пользователь удалил сообщение из чата или канала

- `user_added` — в чат или канал добавлен или перешёл по ссылке новый пользователь

- `user_removed` — пользователь удалён или покинул чат или канал

## [](https://dev.max.ru/docs-api/objects/Update#%D0%A1%D0%B2%D0%BE%D0%B9%D1%81%D1%82%D0%B2%D0%B0%20%D0%BE%D0%B1%D1%8A%D0%B5%D0%BA%D1%82%D0%B0%20Update)Свойства объекта Update

`update_type`  
string  

`timestamp`  
integer `<int64>  

Unix timestamp в миллисекундах, когда произошло событие

`chat_id`  
integer `<int64>  

ID чата или канала, куда был добавлен бот. Как получить ID — в [разделе «Получение chat_id»](https://dev.max.ru/docs-api#%D0%9F%D0%BE%D0%BB%D1%83%D1%87%D0%B5%D0%BD%D0%B8%D0%B5%20chat_id)

`user`  
object User

Пользователь, добавивший бота в чат

`is_channel`  
boolean  

Указывает, что бот добавлен в канал, а не в чат

## [](https://dev.max.ru/docs-api/objects/Update#%D0%9F%D1%80%D0%B8%D0%BC%D0%B5%D1%80%20%D0%BE%D0%B1%D1%8A%D0%B5%D0%BA%D1%82%D0%B0)Пример объекта

JSON

`{ "update_type": "bot_added", "timestamp": 0, "chat_id": 0, "user": { ... }, "is_channel": true }`