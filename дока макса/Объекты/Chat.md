# Chat

Объект содержит общую информацию о групповом чате или канале: его тип, настройки отображения (название, аватар, описание, ссылка), публичную доступность, а также информацию об участниках (владельце, боте и других пользователях), времени их последней активности и событиях

`chat_id`  
integer `<int64>  

ID чата или канала — в зависимости от ограничений метода и от того, с чем вы работаете. Как получить ID — в [разделе «Получение chat_id»](https://dev.max.ru/docs-api#%D0%9F%D0%BE%D0%BB%D1%83%D1%87%D0%B5%D0%BD%D0%B8%D0%B5%20chat_id)

`type`  
enum ChatType

Возможные значения в enum: `"chat"` `"channel"` `"dialog"`

Тип чата:

- `"chat"` — Групповой чат
- `"channel"` — Канал
- `"dialog"` — Диалог

`status`  
enum ChatStatus

Возможные значения в enum: `"active"` `"removed"` `"left"` `"closed"`

Статус чата:

- `"active"` — Бот является активным участником чата
- `"removed"` — Бот был удалён из чата
- `"left"` — Бот покинул чат
- `"closed"` — Чат был закрыт

`title`  
string Nullable  

Отображаемое название чата или канала. Может быть `null` для диалогов

`icon`  
object Image Nullable

Аватар группового чата или канала

`last_event_time`  
integer `<int64>  

Время последнего события в чате или канале в формате Unix timestamp в миллисекундах

`participants_count`  
integer `<int32>  

Количество участников чата или канала. Для диалогов всегда `2`

`owner_id`  
integer `<int64> Nullable optional  

ID владельца чата или канала

`participants`  
object Nullable optional  

Список участников в формате ключ-значение, где ключ — идентификатор участника `user_id`, а значение — время его последней активности в чате или канале `last_event_time`. Может быть `null`, если запрашивается список чатов

`is_public`  
boolean  

Параметр показывает, доступен ли групповой чат или канал публично. Для диалогов и приватных каналов — всегда `false`

`link`  
string Nullable optional  

Ссылка на чат

`description`  
string Nullable  

Описание чата или канала

`dialog_with_user`  
object UserWithPhoto Nullable optional

Данные о пользователе в диалоге (только для чатов типа `"dialog"`)

`messages_count`  
integer Nullable optional  

Количество сообщений в групповом чате или постов канале

`pinned_message`  
object Message Nullable optional

Закреплённое сообщение в чате (возвращается только при запросе конкретного чата или канала)

## [](https://dev.max.ru/docs-api/objects/Chat#%D0%9F%D1%80%D0%B8%D0%BC%D0%B5%D1%80%20%D0%BE%D0%B1%D1%8A%D0%B5%D0%BA%D1%82%D0%B0)Пример объекта

JSON

`{ "chat_id": 0, "type": "chat", "status": "active", "title": "string", "icon": { ... }, "last_event_time": 0, "participants_count": 0, "owner_id": 0, "participants": object, "is_public": true, "link": "string", "description": "string", "dialog_with_user": { ... }, "messages_count": 0, "pinned_message": { ... } }`